// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gitindex

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/zoekt"
	"github.com/google/zoekt/build"

	git "github.com/libgit2/git2go"
)

// RepoModTime returns the time of last fetch of a git repository.
func RepoModTime(dir string) (time.Time, error) {
	var last time.Time
	refDir := filepath.Join(dir, "refs")
	if _, err := os.Lstat(refDir); err == nil {
		if err := filepath.Walk(refDir,
			func(name string, fi os.FileInfo, err error) error {
				if !fi.IsDir() && last.Before(fi.ModTime()) {
					last = fi.ModTime()
				}
				return nil
			}); err != nil {
			return last, err
		}
	}

	// git gc compresses refs into the following file:
	for _, fn := range []string{"info/refs", "packed-refs"} {
		if fi, err := os.Lstat(filepath.Join(dir, fn)); err == nil && !fi.IsDir() && last.Before(fi.ModTime()) {
			last = fi.ModTime()
		}
	}

	return last, nil
}

// FindGitRepos finds git repositories and returns repodir => name map.
func FindGitRepos(arg string) (map[string]string, error) {
	arg, err := filepath.Abs(arg)
	if err != nil {
		return nil, err
	}
	var dirs []string
	gitDirs := map[string]string{}
	if err := filepath.Walk(arg, func(name string, fi os.FileInfo, err error) error {
		if fi, err := os.Lstat(filepath.Join(name, ".git")); err == nil && fi.IsDir() {
			dirs = append(dirs, filepath.Join(name, ".git"))
			return filepath.SkipDir
		}

		if !strings.HasSuffix(name, ".git") || !fi.IsDir() {
			return nil
		}

		fi, err = os.Lstat(filepath.Join(name, "objects"))
		if err != nil || !fi.IsDir() {
			return nil
		}

		dirs = append(dirs, name)
		return filepath.SkipDir
	}); err != nil {
		return nil, err
	}

	for _, dir := range dirs {
		name := strings.TrimSuffix(dir, ".git")
		name = strings.TrimSuffix(name, "/")
		name = strings.TrimPrefix(name, arg)
		name = strings.TrimPrefix(name, "/")
		gitDirs[dir] = name
	}

	return gitDirs, nil
}

type templates struct {
	repo, commit, file, line string
}

// guessRepoURL guesses the URL template for a repo mirrored from a
// well-known git hosting site.
func guessRepoURL(repoDir string) (*templates, error) {
	base, err := git.NewConfig()
	if err != nil {
		return nil, err
	}
	defer base.Free()
	cfg, err := git.OpenOndisk(base, filepath.Join(repoDir, "config"))
	if err != nil {
		return nil, err
	}
	defer cfg.Free()

	remoteURL, err := cfg.LookupString("remote.origin.url")
	if err != nil {
		return nil, err
	}

	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(parsed.Host, "googlesource.com") {
		/// eg. https://gerrit.googlesource.com/gitiles/+/master/tools/run_dev.sh#20
		return &templates{
			repo:   remoteURL,
			commit: remoteURL + "/+/{{.Version}}",
			file:   remoteURL + "/+/{{.Branch}}/{{.Path}}",
			line:   "{{.LineNumber}}",
		}, nil
	} else if parsed.Host == "github.com" {
		// CloneURL from the JSON API has .git
		parsed.Path = strings.TrimSuffix(parsed.Path, ".git")

		// eg. https://github.com/hanwen/go-fuse/blob/notify/genversion.sh#L10
		return &templates{
			repo:   parsed.String(),
			commit: parsed.String() + "/commit/{{.Version}}",
			file:   parsed.String() + "/blob/{{.Branch}}/{{.Path}}",
			line:   "L{{.LineNumber}}",
		}, nil
	}

	return nil, fmt.Errorf("scheme unknown for URL %s", remoteURL)
}

// getCommit returns a tree object for the given reference.
func getCommit(repo *git.Repository, ref string) (*git.Commit, error) {
	obj, err := repo.RevparseSingle(ref)
	if err != nil {
		return nil, err
	}
	defer obj.Free()

	commitObj, err := obj.Peel(git.ObjectCommit)
	if err != nil {
		return nil, err
	}
	return commitObj.AsCommit()
}

// IndexGitRepo indexes the git repository as specified by the options and arguments.
func IndexGitRepo(opts build.Options, branchPrefix string, branches []string, submodules bool) error {
	repo, err := git.OpenRepository(opts.RepoDir)
	if err != nil {
		return err
	}

	if tpl, err := guessRepoURL(opts.RepoDir); err != nil {
		log.Printf("guessRepoURL(%s): %s", opts.RepoDir, err)
	} else {
		opts.RepoURL = tpl.repo
		opts.FileURLTemplate = tpl.file
		opts.CommitURLTemplate = tpl.commit
		opts.LineFragmentTemplate = tpl.line
	}

	// name => branch
	allfiles := map[string][]string{}

	var names []string

	// branch => name => sha1
	data := map[string]map[string]git.Oid{}
	repos := map[git.Oid]*git.Repository{}
	for _, b := range branches {
		fullName := b
		if b != "HEAD" {
			fullName = filepath.Join(branchPrefix, b)
		} else {
			_, ref, err := repo.RevparseExt(b)
			if err != nil {
				return err
			}

			fullName = ref.Name()
			b = strings.TrimPrefix(fullName, branchPrefix)
		}
		commit, err := getCommit(repo, fullName)
		if err != nil {
			return err
		}
		defer commit.Free()
		opts.Branches = append(opts.Branches, build.Branch{
			Name:    b,
			Version: commit.Id().String(),
		})

		tree, err := commit.Tree()
		if err != nil {
			return err
		}
		defer tree.Free()
		fs, subRepos, err := TreeToFiles(repo, tree, submodules)
		if err != nil {
			return err
		}
		for k, v := range subRepos {
			repos[k] = v
		}

		for f := range fs {
			allfiles[f] = append(allfiles[f], b)
		}
		data[b] = fs
	}

	builder, err := build.NewBuilder(opts)
	if err != nil {
		return err
	}

	for n := range allfiles {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		shas := map[git.Oid][]string{}
		for _, b := range allfiles[n] {
			shas[data[b][n]] = append(shas[data[b][n]], b)
		}

		for sha, branches := range shas {
			r := repos[sha]
			if r == nil {
				return fmt.Errorf("no repo found for %s (%s)", n, branches)
			}
			blob, err := r.LookupBlob(&sha)
			if err != nil {
				return err
			}

			if blob.Size() > int64(opts.SizeMax) {
				continue
			}

			builder.Add(zoekt.Document{
				Name:     n,
				Content:  blob.Contents(),
				Branches: branches,
			})
		}
	}
	builder.Finish()

	return nil
}
