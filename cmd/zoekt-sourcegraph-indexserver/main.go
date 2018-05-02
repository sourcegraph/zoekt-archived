// zoekt-sourcegraph-indexserver periodically reindexes enabled repositories on sourcegraph
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/net/trace"

	"github.com/google/zoekt"
	"github.com/google/zoekt/build"
)

func loggedRun(tr trace.Trace, cmd *exec.Cmd) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.Stdout = out
	cmd.Stderr = errOut

	tr.LazyPrintf("%s", cmd.Args)
	if err := cmd.Run(); err != nil {
		outS := out.String()
		errS := errOut.String()
		tr.LazyPrintf("failed: %v", err)
		tr.LazyPrintf("stdout: %s", outS)
		tr.LazyPrintf("stderr: %s", errS)
		tr.SetError()
		log.Printf("command %s failed: %v\nOUT: %s\nERR: %s",
			cmd.Args, err, outS, errS)
	} else {
		tr.LazyPrintf("success")
	}
}

func refresh(root *url.URL, indexDir string, interval time.Duration, cpuFraction float64) {
	cpuCount := int(math.Round(float64(runtime.NumCPU()) * cpuFraction))
	if cpuCount < 1 {
		cpuCount = 1
	}

	t := time.NewTicker(interval)
	for {
		repos, err := listRepos(root)
		if err != nil {
			log.Println(err)
			<-t.C
			continue
		}

		for _, name := range repos {
			tr := trace.New("index", name)

			commit, err := resolveRevision(root, name, "HEAD")
			if err != nil || commit == "" {
				if os.IsNotExist(err) {
					// If we get to this point, it means we have an empty
					// repository (ie we know it exists). As such, we just
					// create an empty shard.
					tr.LazyPrintf("empty repository")
					createEmptyShard(tr, indexDir, name)
					tr.Finish()
					continue
				}
				log.Printf("failed to resolve revision HEAD for %v: %v", name, err)
				tr.LazyPrintf("%v", err)
				tr.Finish()
				continue
			}

			cmd := exec.Command("zoekt-archive-index",
				fmt.Sprintf("-parallelism=%d", cpuCount),
				"-index", indexDir,
				"-incremental",
				"-branch", "HEAD",
				"-commit", commit,
				"-name", name,
				tarballURL(root, name, commit))
			// Prevent prompting
			cmd.Stdin = &bytes.Buffer{}
			loggedRun(tr, cmd)
			tr.Finish()
		}

		if len(repos) == 0 {
			log.Printf("no repos found")
		} else {
			// Only delete shards if we found repositories
			exists := make(map[string]bool)
			for _, name := range repos {
				exists[name] = true
			}
			deleteStaleIndexes(indexDir, exists)
		}

		<-t.C
	}
}

func createEmptyShard(tr trace.Trace, indexDir, name string) {
	cmd := exec.Command("zoekt-archive-index",
		"-index", indexDir,
		"-incremental",
		"-branch", "HEAD",
		"-commit", "404aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"-name", name,
		"-")
	// Empty archive
	cmd.Stdin = bytes.NewBuffer(bytes.Repeat([]byte{0}, 1024))
	loggedRun(tr, cmd)
}

func listRepos(root *url.URL) ([]string, error) {
	u := root.ResolveReference(&url.URL{Path: "/.internal/repos/list"})
	resp, err := http.Post(u.String(), "application/json; charset=utf8", bytes.NewReader([]byte(`{"Enabled": true}`)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list repositories: status %s", resp.Status)
	}

	var data []struct {
		URI string
	}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		return nil, err
	}

	repos := make([]string, len(data))
	for i, r := range data {
		repos[i] = r.URI
	}
	return repos, nil
}

func resolveRevision(root *url.URL, repo, spec string) (string, error) {
	u := root.ResolveReference(&url.URL{Path: fmt.Sprintf("/.internal/git/%s/resolve-revision/%s", repo, spec)})
	resp, err := http.Get(u.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to resolve revision %s@%s: status %s", repo, spec, resp.Status)
	}

	var b bytes.Buffer
	_, err = b.ReadFrom(resp.Body)
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

func tarballURL(root *url.URL, repo, commit string) string {
	return root.ResolveReference(&url.URL{Path: fmt.Sprintf("/.internal/git/%s/tar/%s", repo, commit)}).String()
}

// deleteIfStale deletes the shard if its corresponding repo name is not in
// exists.
func deleteIfStale(exists map[string]bool, fn string) error {
	f, err := os.Open(fn)
	if err != nil {
		return nil
	}
	defer f.Close()

	ifile, err := zoekt.NewIndexFile(f)
	if err != nil {
		return nil
	}
	defer ifile.Close()

	repo, _, err := zoekt.ReadMetadata(ifile)
	if err != nil {
		return nil
	}

	if !exists[repo.Name] {
		log.Printf("%s no longer exists, deleting %s", repo.Name, fn)
		return os.Remove(fn)
	}

	return nil
}

func deleteStaleIndexes(indexDir string, exists map[string]bool) {
	expr := indexDir + "/*"
	fs, err := filepath.Glob(expr)
	if err != nil {
		log.Printf("Glob(%q): %v", expr, err)
	}

	for _, f := range fs {
		if err := deleteIfStale(exists, f); err != nil {
			log.Printf("deleteIfStale(%q): %v", f, err)
		}
	}
}

func main() {
	root := flag.String("sourcegraph_url", "", "http://sourcegraph-frontend-internal or http://localhost:3090")
	interval := flag.Duration("interval", 10*time.Minute, "sync with sourcegraph this often")
	index := flag.String("index", build.DefaultDir, "set index directory to use")
	listen := flag.String("listen", "", "listen on this address.")
	cpuFraction := flag.Float64("cpu_fraction", 0.25,
		"use this fraction of the cores for indexing.")
	flag.Parse()

	if *cpuFraction <= 0.0 || *cpuFraction > 1.0 {
		log.Fatal("cpu_fraction must be between 0.0 and 1.0")
	}
	if *index == "" {
		log.Fatal("must set -index")
	}
	if *root == "" {
		log.Fatal("must set -sourcegraph_url")
	}
	rootURL, err := url.Parse(*root)
	if err != nil {
		log.Fatalf("url.Parse(%v): %v", *root, err)
	}

	// Automatically prepend our own path at the front, to minimize
	// required configuration.
	if l, err := os.Readlink("/proc/self/exe"); err == nil {
		os.Setenv("PATH", filepath.Dir(l)+":"+os.Getenv("PATH"))
	}

	if _, err := os.Stat(*index); err != nil {
		if err := os.MkdirAll(*index, 0755); err != nil {
			log.Fatalf("MkdirAll %s: %v", *index, err)
		}
	}

	if *listen != "" {
		go func() {
			trace.AuthRequest = func(req *http.Request) (any, sensitive bool) {
				return true, true
			}
			log.Printf("serving HTTP on %s", *listen)
			log.Fatal(http.ListenAndServe(*listen, http.HandlerFunc(trace.Traces)))
		}()
	}

	refresh(rootURL, *index, *interval, *cpuFraction)
}
