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
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/libgit2/git2go"
)

type RepoCache struct {
	baseDir string

	reposMu sync.Mutex
	repos   map[string]*git.Repository
}

func NewRepoCache(dir string) *RepoCache {
	return &RepoCache{
		baseDir: dir,
		repos:   make(map[string]*git.Repository),
	}
}

func (rc *RepoCache) Close() {
	rc.reposMu.Lock()
	defer rc.reposMu.Unlock()
	for _, v := range rc.repos {
		v.Free()
	}
}

func (rc *RepoCache) Open(u *url.URL) (*git.Repository, error) {
	key := filepath.Join(u.Host, u.Path)
	if !strings.HasSuffix(key, ".git") {
		key += ".git"
	}

	rc.reposMu.Lock()
	defer rc.reposMu.Unlock()

	r := rc.repos[key]
	if r != nil {
		return r, nil
	}

	d := filepath.Join(rc.baseDir, key)
	repo, err := git.OpenRepository(d)
	if err == nil {
		rc.repos[key] = repo
	}
	return repo, err
}