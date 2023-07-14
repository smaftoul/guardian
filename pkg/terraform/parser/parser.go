// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/abcxyz/guardian/pkg/assetinventory"
	"github.com/abcxyz/guardian/pkg/iam"
	"github.com/abcxyz/guardian/pkg/storage"
)

const (
	// UnknownParentID is used when we are unable to find a match for the asset parent (e.g. project, folder, org)
	// This shouldn't happen but it is theoretically possible especially if there is a race condition between
	// fetching the projects & folders and querying for terraform state.
	UnknownParentID = "UNKNOWN_PARENT_ID"
	// UnknownParentType is used when we cannot find an asset parent. See UnknownParentID.
	UnknownParentType = "UNKNOWN_PARENT_TYPE"
	// Default max size for a terraform statefile is 512 MB.
	defaultTerraformStateFileSizeLimit = 512 * 1024 * 1024 // 512 MB
)

// ResourceInstances represents the JSON terraform state IAM instance.
type ResourceInstance struct {
	Attributes struct {
		ID      string   `json:"id"`
		Members []string `json:"members,omitempty"`
		Member  string   `json:"member,omitempty"`
		Folder  string   `json:"folder,omitempty"`
		Project string   `json:"project,omitempty"`
		Role    string   `json:"role,omitempty"`
	}
}

// TerraformState represents the JSON terraform state.
type TerraformState struct {
	Resources []struct {
		Type      string             `json:"type"`
		Instances []ResourceInstance `json:"instances"`
	} `json:"resources"`
}

// Terraform defines the common terraform functionality.
type Terraform interface {
	// SetAssets sets the assets to use for GCP asset lookup.
	SetAssets(gcpFolders, gcpProjects map[string]*assetinventory.HierarchyNode)
	// StateFileURIs returns the URIs of terraform state files located in the given GCS buckets.
	StateFileURIs(ctx context.Context, gcsBuckets []string) ([]string, error)
	// ProcessStates returns the IAM permissions stored in the given state files.
	ProcessStates(ctx context.Context, gcsUris []string) ([]*iam.AssetIAM, error)
}

type TerraformParser struct {
	GCS               storage.Storage
	OrganizationID    string
	gcpAssetsByID     map[string]*assetinventory.HierarchyNode
	gcpFoldersByName  map[string]*assetinventory.HierarchyNode
	gcpProjectsByName map[string]*assetinventory.HierarchyNode
}

// NewTerraformParser creates a new terraform parser.
func NewTerraformParser(ctx context.Context, organizationID string) (*TerraformParser, error) {
	client, err := storage.NewGoogleCloudStorage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize gcs Client: %w", err)
	}
	return &TerraformParser{
		GCS:               client,
		gcpAssetsByID:     make(map[string]*assetinventory.HierarchyNode),
		gcpFoldersByName:  make(map[string]*assetinventory.HierarchyNode),
		gcpProjectsByName: make(map[string]*assetinventory.HierarchyNode),
		OrganizationID:    organizationID,
	}, nil
}

// SetAssets sets up the assets to use when looking up IAM asset bindings.
func (p *TerraformParser) SetAssets(
	gcpFolders map[string]*assetinventory.HierarchyNode,
	gcpProjects map[string]*assetinventory.HierarchyNode,
) {
	p.gcpAssetsByID = assetinventory.Merge(gcpFolders, gcpProjects)
	p.gcpFoldersByName = assetinventory.AssetsByName(gcpFolders)
	p.gcpProjectsByName = assetinventory.AssetsByName(gcpProjects)
}

// StateFileURIs finds all terraform state files in the given buckets.
func (p *TerraformParser) StateFileURIs(ctx context.Context, gcsBuckets []string) ([]string, error) {
	var gcsURIs []string
	for _, bucket := range gcsBuckets {
		allStateFiles, err := p.GCS.ObjectsWithName(ctx, bucket, "default.tfstate")
		if err != nil {
			return nil, fmt.Errorf("failed to determine state files in GCS bucket %s: %w", bucket, err)
		}
		gcsURIs = append(gcsURIs, allStateFiles...)
	}
	return gcsURIs, nil
}

// ProcessStates finds all IAM in memberships, bindings, or policies in the given terraform state files.
func (p *TerraformParser) ProcessStates(ctx context.Context, gcsUris []string) ([]*iam.AssetIAM, error) {
	var iams []*iam.AssetIAM
	for _, uri := range gcsUris {
		var state TerraformState
		bucket, name, err := storage.SplitObjectURI(uri)
		if err != nil {
			return nil, fmt.Errorf("failed to parse GCS URI: %w", err)
		}
		r, err := p.GCS.DownloadObject(ctx, *bucket, *name)
		if err != nil {
			return nil, fmt.Errorf("failed to download gcs URI for terraform: %w", err)
		}
		defer r.Close()
		lr := io.LimitReader(r, defaultTerraformStateFileSizeLimit)
		if err := json.NewDecoder(lr).Decode(&state); err != nil {
			return nil, fmt.Errorf("failed to decode terraform state: %w", err)
		}
		iams = append(iams, p.parseTerraformStateIAM(state)...)
	}
	return iams, nil
}

func (p *TerraformParser) parseTerraformStateIAM(state TerraformState) []*iam.AssetIAM {
	var iams []*iam.AssetIAM
	for _, r := range state.Resources {
		if strings.Contains(r.Type, "google_organization_iam_binding") {
			iams = append(iams, p.parseIAMBindingForOrg(r.Instances)...)
		} else if strings.Contains(r.Type, "google_folder_iam_binding") {
			iams = append(iams, p.parseIAMBindingForFolder(r.Instances)...)
		} else if strings.Contains(r.Type, "google_project_iam_binding") {
			iams = append(iams, p.parseIAMBindingForProject(r.Instances)...)
		}

		if strings.Contains(r.Type, "google_organization_iam_member") {
			iams = append(iams, p.parseIAMMemberForOrg(r.Instances)...)
		} else if strings.Contains(r.Type, "google_folder_iam_member") {
			iams = append(iams, p.parseIAMMemberForFolder(r.Instances)...)
		} else if strings.Contains(r.Type, "google_project_iam_member") {
			iams = append(iams, p.parseIAMMemberForProject(r.Instances)...)
		}
	}
	return iams
}

func (p *TerraformParser) parseIAMBindingForOrg(instances []ResourceInstance) []*iam.AssetIAM {
	var iams []*iam.AssetIAM
	for _, i := range instances {
		for _, m := range i.Attributes.Members {
			iams = append(iams, &iam.AssetIAM{
				Member:       m,
				Role:         i.Attributes.Role,
				ResourceID:   p.OrganizationID,
				ResourceType: assetinventory.Organization,
			})
		}
	}
	return iams
}

func (p *TerraformParser) parseIAMBindingForFolder(instances []ResourceInstance) []*iam.AssetIAM {
	var iams []*iam.AssetIAM
	for _, i := range instances {
		for _, m := range i.Attributes.Members {
			folderID := strings.TrimPrefix(i.Attributes.Folder, "folders/")
			parentID, parentType := p.maybeFindGCPAssetIDAndType(folderID)
			iams = append(iams, &iam.AssetIAM{
				Member:       m,
				Role:         i.Attributes.Role,
				ResourceID:   parentID,
				ResourceType: parentType,
			})
		}
	}
	return iams
}

func (p *TerraformParser) parseIAMBindingForProject(instances []ResourceInstance) []*iam.AssetIAM {
	var iams []*iam.AssetIAM
	for _, i := range instances {
		for _, m := range i.Attributes.Members {
			parentID, parentType := p.maybeFindGCPAssetIDAndType(i.Attributes.Project)
			iams = append(iams, &iam.AssetIAM{
				Member:       m,
				Role:         i.Attributes.Role,
				ResourceID:   parentID,
				ResourceType: parentType,
			})
		}
	}
	return iams
}

func (p *TerraformParser) parseIAMMemberForOrg(instances []ResourceInstance) []*iam.AssetIAM {
	iams := make([]*iam.AssetIAM, len(instances))
	for x, i := range instances {
		iams[x] = &iam.AssetIAM{
			Member:       i.Attributes.Member,
			Role:         i.Attributes.Role,
			ResourceID:   p.OrganizationID,
			ResourceType: assetinventory.Organization,
		}
	}
	return iams
}

func (p *TerraformParser) parseIAMMemberForFolder(instances []ResourceInstance) []*iam.AssetIAM {
	iams := make([]*iam.AssetIAM, len(instances))
	for x, i := range instances {
		folderID := strings.TrimPrefix(i.Attributes.Folder, "folders/")
		parentID, parentType := p.maybeFindGCPAssetIDAndType(folderID)
		iams[x] = &iam.AssetIAM{
			Member:       i.Attributes.Member,
			Role:         i.Attributes.Role,
			ResourceID:   parentID,
			ResourceType: parentType,
		}
	}
	return iams
}

func (p *TerraformParser) parseIAMMemberForProject(instances []ResourceInstance) []*iam.AssetIAM {
	iams := make([]*iam.AssetIAM, len(instances))
	for x, i := range instances {
		parentID, parentType := p.maybeFindGCPAssetIDAndType(i.Attributes.Project)
		iams[x] = &iam.AssetIAM{
			Member:       i.Attributes.Member,
			Role:         i.Attributes.Role,
			ResourceID:   parentID,
			ResourceType: parentType,
		}
	}
	return iams
}

func (p *TerraformParser) maybeFindGCPAssetIDAndType(ID string) (string, string) {
	asset := p.findGCPAsset(ID)
	if asset == nil {
		return UnknownParentID, UnknownParentType
	}
	return asset.ID, asset.NodeType
}

// findGCPAsset attempts to find a gcp asset match for the ID.
func (p *TerraformParser) findGCPAsset(gcpAssetID string) *assetinventory.HierarchyNode {
	if _, err := strconv.ParseInt(gcpAssetID, 10, 64); err == nil {
		if _, ok := p.gcpAssetsByID[gcpAssetID]; !ok {
			return nil
		}
		return p.gcpAssetsByID[gcpAssetID]
	} else {
		if _, ok := p.gcpFoldersByName[gcpAssetID]; ok {
			return p.gcpFoldersByName[gcpAssetID]
		} else if _, ok := p.gcpProjectsByName[gcpAssetID]; ok {
			return p.gcpProjectsByName[gcpAssetID]
		}
		return nil
	}
}
