package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/drone/go-scm/scm"
	"github.com/drone/go-scm/scm/driver/github"
	"github.com/forensicanalysis/gitfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"
)

func main() {
	prNum, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Printf("Could not parse PR number %q: %s", os.Args[1], err)
	}
	pr, err := GetPullRequest(os.Getenv("GITHUB_REPOSITORY"), prNum)
	if err != nil {
		fmt.Println("Failed to get pull request:", err)
		os.Exit(1)
	}
	gitRepo, err := GitRepoFS(pr)
	if err != nil {
		fmt.Println("Failed to load git repo:", err)
		os.Exit(2)
	}

	req, err := RequiredOwners(pr, gitRepo)
	if err != nil {
		fmt.Println("Failed to find ownership, error:", err)
		os.Exit(1)
	}
	fmt.Println("Checking", pr)

	if len(req.NeedsApprove) == 0 {
		fmt.Println("Hello, world, all is okay!")
	} else {
		fmt.Println("Oops, I broke it, the following files need approval:", req.NeedsApprove)
	}
}

// A simplified PullRequest -- a set of file paths and the author(s) of the PR.
type PullRequest struct {
	Server string
	Repo   string
	Id     int

	Files sets.String

	Author string
}

func (pr PullRequest) String() string {
	preamble := []string{fmt.Sprintf("PR #%d: %d files (%s):", pr.Id, len(pr.Files), pr.Author)}
	return strings.Join(append(preamble, pr.Files.List()...), "\n - ")
}

func GetPullRequest(repo string, pull int) (PullRequest, error) {
	retval := PullRequest{
		Server: "github.com",
		Repo:   repo,
		Id:     pull,
		Files:  sets.NewString(),
	}
	client, err := github.New("https://api.github.com")
	if err != nil {
		return retval, err
	}
	ctx := context.Background()

	pr, resp, err := client.PullRequests.Find(ctx, retval.Repo, retval.Id)
	if err != nil {
		return retval, err
	}
	retval.Author = pr.Author.Login

	// TODO: paging
	opts := scm.ListOptions{
		Page: 0,
		Size: 100,
	}
	files, resp, err := client.PullRequests.ListChanges(ctx, retval.Repo, retval.Id, opts)
	if err != nil {
		return retval, err
	}
	if resp.Page.Last > opts.Page {
		fmt.Printf("More than %d results, only rendering pages %d of %d\n", len(files), opts.Page, resp.Page.Last)
	}
	for _, change := range files {
		retval.Files.Insert(change.Path)
	}

	return retval, nil
}

func GitRepoFS(pr PullRequest) (fs.FS, error) {
	url := fmt.Sprintf("https://%s/%s.git", pr.Server, pr.Repo)
	return gitfs.NewWithOptions(&git.CloneOptions{
		URL:           url,
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
	Options Options `yaml:"options,omitempty"`
	Config  `yaml:",inline"`
	// Additional reviewers, mapping Regex pattern to *additional* Config
	Filters map[string]Config `yaml:"filters,omitempty"`
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

	for _, a := range ownersFile.Approvers {
		if pr.Author == a {
			result.NeedsReview = pr.Files
			result.NeedsApprove = nil
			return &result, nil
		}
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
		authorIsApprover := false
		for _, a := range config.Approvers {
			if pr.Author == a {
				authorIsApprover = true
				break
			}
		}
		if authorIsApprover {
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
			res, err := RequiredOwners(PullRequest{Files: files, Author: pr.Author}, subDir)
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
