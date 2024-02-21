// Copyright 2023 The Authors (see AUTHORS file)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package entrypoints

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/abcxyz/guardian/pkg/git"
	"github.com/abcxyz/pkg/logging"
	"github.com/abcxyz/pkg/testutil"
)

func TestEntrypointsProcess(t *testing.T) {
	t.Parallel()

	ctx := logging.WithLogger(context.Background(), logging.TestLogger(t))

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name                  string
		directory             string
		flagIsGitHubActions   bool
		flagGitHubOwner       string
		flagGitHubRepo        string
		flagPullRequestNumber int
		flagDestRef           string
		flagSourceRef         string
		flagDetectChanges     bool
		flagMaxDepth          int
		flagFormat            string
		gitClient             *git.MockGitClient
		err                   string
		expStdout             string
		expStderr             string
	}{
		{
			name:                  "success",
			directory:             "testdata",
			flagFormat:            "text",
			flagIsGitHubActions:   true,
			flagGitHubOwner:       "owner",
			flagGitHubRepo:        "repo",
			flagPullRequestNumber: 1,
			flagDestRef:           "main",
			flagSourceRef:         "ldap/feature",
			flagDetectChanges:     true,
			gitClient: &git.MockGitClient{
				DiffResp: []string{
					path.Join(cwd, "testdata/backends/project1"),
					path.Join(cwd, "testdata/backends/project2"),
				},
			},
			expStdout: "testdata/backends/project1\ntestdata/backends/project2",
		},
		{
			name:                  "returns_json",
			directory:             "testdata",
			flagFormat:            "json",
			flagIsGitHubActions:   true,
			flagGitHubOwner:       "owner",
			flagGitHubRepo:        "repo",
			flagPullRequestNumber: 3,
			flagDestRef:           "main",
			flagSourceRef:         "ldap/feature",
			flagDetectChanges:     true,
			gitClient: &git.MockGitClient{
				DiffResp: []string{
					path.Join(cwd, "testdata/backends/project1"),
					path.Join(cwd, "testdata/backends/project2"),
				},
			},
			expStdout: "[\"testdata/backends/project1\",\"testdata/backends/project2\"]",
		},
		{
			name:                  "invalid_format",
			directory:             "testdata",
			flagFormat:            "yaml",
			flagIsGitHubActions:   true,
			flagGitHubOwner:       "owner",
			flagGitHubRepo:        "repo",
			flagPullRequestNumber: 3,
			flagDestRef:           "main",
			flagSourceRef:         "ldap/feature",
			flagDetectChanges:     true,
			gitClient: &git.MockGitClient{
				DiffResp: []string{
					path.Join(cwd, "testdata/backends/project1"),
					path.Join(cwd, "testdata/backends/project2"),
				},
			},
			err: "invalid format flag: yaml",
		},
		{
			name:                  "skips_detect_changes",
			directory:             "testdata",
			flagFormat:            "text",
			flagIsGitHubActions:   true,
			flagGitHubOwner:       "owner",
			flagGitHubRepo:        "repo",
			flagPullRequestNumber: 1,
			flagDestRef:           "main",
			flagSourceRef:         "ldap/feature",
			flagDetectChanges:     false,
			gitClient:             &git.MockGitClient{},
			expStdout:             "testdata/backends/project1\ntestdata/backends/project2",
		},
		{
			name:                  "errors",
			directory:             "testdata",
			flagFormat:            "text",
			flagIsGitHubActions:   true,
			flagGitHubOwner:       "owner",
			flagGitHubRepo:        "repo",
			flagPullRequestNumber: 2,
			flagDestRef:           "main",
			flagSourceRef:         "ldap/feature",
			flagDetectChanges:     true,
			gitClient: &git.MockGitClient{
				DiffErr: fmt.Errorf("failed to run git diff"),
			},
			err: "failed to find git diff directories: failed to run git diff",
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := &EntrypointsCommand{
				directory: tc.directory,

				flagFormat:        tc.flagFormat,
				flagDestRef:       tc.flagDestRef,
				flagSourceRef:     tc.flagSourceRef,
				flagDetectChanges: tc.flagDetectChanges,
				flagMaxDepth:      tc.flagMaxDepth,
				gitClient:         tc.gitClient,
			}

			_, stdout, stderr := c.Pipe()

			err := c.Process(ctx)
			if diff := testutil.DiffErrString(err, tc.err); diff != "" {
				t.Errorf(diff)
			}

			if got, want := strings.TrimSpace(stdout.String()), strings.TrimSpace(tc.expStdout); !strings.Contains(got, want) {
				t.Errorf("expected stdout\n\n%s\n\nto contain\n\n%s\n\n", got, want)
			}
			if got, want := strings.TrimSpace(stderr.String()), strings.TrimSpace(tc.expStderr); !strings.Contains(got, want) {
				t.Errorf("expected stderr\n\n%s\n\nto contain\n\n%s\n\n", got, want)
			}
		})
	}
}

func TestAfterParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		err  string
	}{
		{
			name: "validate_refs",
			args: []string{"-format=yaml", "-detect-changes", "-max-depth=0"},
			err:  "invalid flag: source-ref and dest-ref are required to detect changes, to ignore changes set the detect-changes flag",
		},
		{
			name: "validate_format",
			args: []string{"-format=yaml", "-source-ref=a", "-dest-ref=b", "-detect-changes", "-max-depth=0"},
			err:  "invalid flag: format yaml (supported formats are: [json text])",
		},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := EntrypointsCommand{}

			f := c.Flags()
			err := f.Parse(tc.args)
			if diff := testutil.DiffErrString(err, tc.err); diff != "" {
				t.Errorf(diff)
			}
		})
	}
}
