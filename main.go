package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/forensicanalysis/gitfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"
)

func main() {
	pr, err := GetPullRequest()
	if err != nil {
		fmt.Println("Failed to get pull request:", err)
		os.Exit(1)
	}
	gitRepo, err := GitRepoFS()
	if err != nil {
		fmt.Println("Failed to load git repo:", err)
		os.Exit(2)
	}

	req, err := RequiredOwners(pr, gitRepo)
	if err != nil {
		fmt.Println("Failed to find ownership, error:", err)
		os.Exit(1)
	}
	if len(req.NeedsApprove) == 0 {
		fmt.Println("Hello, world, all is okay!")
	} else {
		fmt.Println("Oops, I broke it, the following files need approval:", req.NeedsApprove)
	}
}

// A simplified PullRequest -- a set of file paths and the author(s) of the PR.
type PullRequest struct {
	Files sets.String

	Authors sets.String
}

func GetPullRequest() (PullRequest, error) {
	return PullRequest{
		Files: sets.NewString("foo.go", "bar.go"),
	}, nil
}

func GitRepoFS() (fs.FS, error) {
	return gitfs.NewWithOptions(&git.CloneOptions{
		URL:           os.Getenv("GITHUB_REPOSITORY"),
		ReferenceName: plumbing.NewBranchReferenceName(os.Getenv("GITHUB_BASE_REF")),
		// TODO: auth
		SingleBranch: true,
		Depth:        1,
	})
}

// What review is needed on a given PullRequest. Keys are file paths, values
// in NeedsApprove are GitHub usernames.
type ReviewRequirement struct {
	NeedsReview  sets.String
	NeedsApprove map[string]sets.String
}

// Copied from github.com/kubernetes/test-infra/prow/repoowners/repoowners.go
type Config struct {
	Approvers         []string `yaml:"approvers,omitempty"`
	Reviewers         []string `yaml:"reviewers,omitempty"`
	RequiredReviewers []string `yaml:"required_reviewers,omitempty"`
	Labels            []string `yaml:"labels,omitempty"`
}

type Options struct {
}

type OwnersFile struct {
	Options Options `json:"options,omitempty"`
	Config  `json:",inline"`
	// Additional reviewers, mapping Regex pattern to *additional* Config
	Filters map[string]Config `json:"filters,omitempty"`
}

// RequiredOwners considers a PullRequest in the context of a git repo represented by an fs,
// and returns
func RequiredOwners(pr PullRequest, gitFs fs.FS) (*ReviewRequirement, error) {
	result := ReviewRequirement{
		NeedsApprove: make(map[string]sets.String),
	}
	baseOwners, err := gitFs.Open("OWNERS")
	if err != nil {
		if os.IsNotExist(err) {
			result.NeedsApprove = make(map[string]sets.String, len(pr.Files))
			for file := range pr.Files {
				result.NeedsApprove[file] = nil
			}
			return &result, nil
		}
		return nil, err
	}

	var ownersFile OwnersFile
	if err := yaml.NewDecoder(baseOwners).Decode(&ownersFile); err != nil {
		return nil, err
	}

	if pr.Authors.HasAny(ownersFile.Approvers...) {
		result.NeedsReview = pr.Files
		result.NeedsApprove = nil
		return &result, nil
	}

	for f := range pr.Files {
		result.NeedsApprove[f] = sets.NewString(ownersFile.Approvers...)
	}

	for filter, config := range ownersFile.Filters {
		re, err := regexp.Compile(filter)
		if err != nil {
			// TODO: complain
			continue
		}
		if pr.Authors.HasAny(config.Approvers...) {
			for file := range result.NeedsApprove {
				if re.MatchString(file) {
					result.NeedsReview.Insert(file)
					delete(result.NeedsApprove, file)
				}
			}
		} else {
			for file := range result.NeedsApprove {
				if re.MatchString(file) {
					result.NeedsApprove[file].Insert(config.Approvers...)
				}
			}
		}
	}

	// If we have files that aren't already approved, check subdirectory OWNERS files
	if len(result.NeedsApprove) > 0 {
		filesByDir := map[string]sets.String{}
		for f := range result.NeedsApprove {
			dirname, filename, _ := strings.Cut(f, string(os.PathSeparator))
			if filename == "" {
				continue
			}
			filesByDir[dirname].Insert(filename)
		}
		for dir, files := range filesByDir {
			subDir, err := fs.Sub(gitFs, dir)
			if err != nil {
				return nil, err
			}
			res, err := RequiredOwners(PullRequest{Files: files, Authors: pr.Authors}, subDir)
			if err != nil {
				return nil, err
			}

			for f := range res.NeedsReview {
				fullPath := filepath.Join(dir, f)
				result.NeedsReview.Insert(fullPath)
				delete(result.NeedsApprove, fullPath)
			}
			for f, additionalOwners := range res.NeedsApprove {
				fullPath := filepath.Join(dir, f)
				result.NeedsApprove[fullPath].Insert(additionalOwners.List()...)
			}
		}
	}

	return &result, nil
}
