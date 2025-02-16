/*
Copyright The Helm Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package downloader

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"k8s.io/helm/pkg/getter"
	"k8s.io/helm/pkg/helm/helmpath"
	"k8s.io/helm/pkg/provenance"
	"k8s.io/helm/pkg/repo"
	"k8s.io/helm/pkg/urlutil"
)

// VerificationStrategy describes a strategy for determining whether to verify a chart.
type VerificationStrategy int

const (
	// VerifyNever will skip all verification of a chart.
	VerifyNever VerificationStrategy = iota
	// VerifyIfPossible will attempt a verification, it will not error if verification
	// data is missing. But it will not stop processing if verification fails.
	VerifyIfPossible
	// VerifyAlways will always attempt a verification, and will fail if the
	// verification fails.
	VerifyAlways
	// VerifyLater will fetch verification data, but not do any verification.
	// This is to accommodate the case where another step of the process will
	// perform verification.
	VerifyLater
)

// ErrNoOwnerRepo indicates that a given chart URL can't be found in any repos.
var ErrNoOwnerRepo = errors.New("could not find a repo containing the given URL")

// ChartDownloader handles downloading a chart.
//
// It is capable of performing verifications on charts as well.
type ChartDownloader struct {
	// Out is the location to write warning and info messages.
	Out io.Writer
	// Verify indicates what verification strategy to use.
	Verify VerificationStrategy
	// Keyring is the keyring file used for verification.
	Keyring string
	// HelmHome is the $HELM_HOME.
	HelmHome helmpath.Home
	// Getter collection for the operation
	Getters getter.Providers
	// Chart repository username
	Username string
	// Chart repository password
	Password string
}

// DownloadTo retrieves a chart. Depending on the settings, it may also download a provenance file.
//
// If Verify is set to VerifyNever, the verification will be nil.
// If Verify is set to VerifyIfPossible, this will return a verification (or nil on failure), and print a warning on failure.
// If Verify is set to VerifyAlways, this will return a verification or an error if the verification fails.
// If Verify is set to VerifyLater, this will download the prov file (if it exists), but not verify it.
//
// For VerifyNever and VerifyIfPossible, the Verification may be empty.
//
// Returns a string path to the location where the file was downloaded and a verification
// (if provenance was verified), or an error if something bad happened.
func (c *ChartDownloader) DownloadTo(ref, version, dest string) (string, *provenance.Verification, error) {
	u, r, g, err := c.ResolveChartVersionAndGetRepo(ref, version)
	if err != nil {
		return "", nil, err
	}

	data, err := g.Get(u.String())
	if err != nil {
		return "", nil, err
	}

	name := filepath.Base(u.Path)
	destfile := filepath.Join(dest, name)
	if err := ioutil.WriteFile(destfile, data.Bytes(), 0644); err != nil {
		return destfile, nil, err
	}

	// If provenance is requested, verify it.
	ver := &provenance.Verification{}
	if c.Verify > VerifyNever {
		body, err := r.Client.Get(u.String() + ".prov")
		if err != nil {
			if c.Verify == VerifyAlways {
				return destfile, ver, errors.Errorf("failed to fetch provenance %q", u.String()+".prov")
			}
			fmt.Fprintf(c.Out, "WARNING: Verification not found for %s: %s\n", ref, err)
			return destfile, ver, nil
		}
		provfile := destfile + ".prov"
		if err := ioutil.WriteFile(provfile, body.Bytes(), 0644); err != nil {
			return destfile, nil, err
		}

		if c.Verify != VerifyLater {
			ver, err = VerifyChart(destfile, c.Keyring)
			if err != nil {
				// Fail always in this case, since it means the verification step
				// failed.
				return destfile, ver, err
			}
		}
	}
	return destfile, ver, nil
}

// ResolveChartVersion resolves a chart reference to a URL.
//
// It returns the URL as well as a preconfigured repo.Getter that can fetch
// the URL.
//
// A reference may be an HTTP URL, a 'reponame/chartname' reference, or a local path.
//
// A version is a SemVer string (1.2.3-beta.1+f334a6789).
//
//	- For fully qualified URLs, the version will be ignored (since URLs aren't versioned)
//	- For a chart reference
//		* If version is non-empty, this will return the URL for that version
//		* If version is empty, this will return the URL for the latest version
//		* If no version can be found, an error is returned
func (c *ChartDownloader) ResolveChartVersion(ref, version string) (*url.URL, getter.Getter, error) {
	u, r, _, err := c.ResolveChartVersionAndGetRepo(ref, version)
	if r != nil {
		return u, r.Client, err
	}
	return u, nil, err
}

// ResolveChartVersionAndGetRepo is the same as the ResolveChartVersion method, but returns the chart repositoryy.
func (c *ChartDownloader) ResolveChartVersionAndGetRepo(ref, version string) (*url.URL, *repo.ChartRepository, *getter.HTTPGetter, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, nil, nil, errors.Errorf("invalid chart URL format: %s", ref)
	}

	rf, err := repo.LoadFile(c.HelmHome.RepositoryFile())
	if err != nil {
		return u, nil, nil, err
	}

	// TODO add user-agent
	g, err := getter.NewHTTPGetter(ref, "", "", "")
	if err != nil {
		return u, nil, nil, err
	}

	if u.IsAbs() && len(u.Host) > 0 && len(u.Path) > 0 {
		// In this case, we have to find the parent repo that contains this chart
		// URL. And this is an unfortunate problem, as it requires actually going
		// through each repo cache file and finding a matching URL. But basically
		// we want to find the repo in case we have special SSL cert config
		// for that repo.

		rc, err := c.scanReposForURL(ref, rf)
		if err != nil {
			// If there is no special config, return the default HTTP client and
			// swallow the error.
			if err == ErrNoOwnerRepo {
				r := &repo.ChartRepository{}
				r.Client = g
				g.SetCredentials(c.getRepoCredentials(r))
				return u, r, g, err
			}
			return u, nil, nil, err
		}
		r, err := repo.NewChartRepository(rc, c.Getters)
		// If we get here, we don't need to go through the next phase of looking
		// up the URL. We have it already. So we just return.
		return u, r, g, err
	}

	// See if it's of the form: repo/path_to_chart
	p := strings.SplitN(u.Path, "/", 2)
	if len(p) < 2 {
		return u, nil, nil, errors.Errorf("non-absolute URLs should be in form of repo_name/path_to_chart, got: %s", u)
	}

	repoName := p[0]
	chartName := p[1]
	rc, err := pickChartRepositoryConfigByName(repoName, rf.Repositories)

	if err != nil {
		return u, nil, nil, err
	}

	r, err := repo.NewChartRepository(rc, c.Getters)
	if err != nil {
		return u, nil, nil, err
	}
	g.SetCredentials(c.getRepoCredentials(r))

	// Next, we need to load the index, and actually look up the chart.
	i, err := repo.LoadIndexFile(c.HelmHome.CacheIndex(r.Config.Name))
	if err != nil {
		return u, r, g, errors.Wrap(err, "no cached repo found. (try 'helm repo update')")
	}

	cv, err := i.Get(chartName, version)
	if err != nil {
		return u, r, g, errors.Wrapf(err, "chart %q matching %s not found in %s index. (try 'helm repo update')", chartName, version, r.Config.Name)
	}

	if len(cv.URLs) == 0 {
		return u, r, g, errors.Errorf("chart %q has no downloadable URLs", ref)
	}

	// TODO: Seems that picking first URL is not fully correct
	u, err = url.Parse(cv.URLs[0])
	if err != nil {
		return u, r, g, errors.Errorf("invalid chart URL format: %s", ref)
	}

	// If the URL is relative (no scheme), prepend the chart repo's base URL
	if !u.IsAbs() {
		repoURL, err := url.Parse(rc.URL)
		if err != nil {
			return repoURL, r, nil, err
		}
		q := repoURL.Query()
		// We need a trailing slash for ResolveReference to work, but make sure there isn't already one
		repoURL.Path = strings.TrimSuffix(repoURL.Path, "/") + "/"
		u = repoURL.ResolveReference(u)
		u.RawQuery = q.Encode()
		// TODO add user-agent
		g, err := getter.NewHTTPGetter(rc.URL, "", "", "")
		if err != nil {
			return repoURL, r, nil, err
		}
		g.SetCredentials(c.getRepoCredentials(r))
		return u, r, g, err
	}

	return u, r, g, nil
}

// If this ChartDownloader is not configured to use credentials, and the chart repository sent as an argument is,
// then the repository's configured credentials are returned.
// Else, this ChartDownloader's credentials are returned.
func (c *ChartDownloader) getRepoCredentials(r *repo.ChartRepository) (username, password string) {
	username = c.Username
	password = c.Password
	if r != nil && r.Config != nil {
		if username == "" {
			username = r.Config.Username
		}
		if password == "" {
			password = r.Config.Password
		}
	}
	return
}

// VerifyChart takes a path to a chart archive and a keyring, and verifies the chart.
//
// It assumes that a chart archive file is accompanied by a provenance file whose
// name is the archive file name plus the ".prov" extension.
func VerifyChart(path, keyring string) (*provenance.Verification, error) {
	// For now, error out if it's not a tar file.
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if fi.IsDir() {
		return nil, errors.New("unpacked charts cannot be verified")
	} else if !isTar(path) {
		return nil, errors.New("chart must be a tgz file")
	}

	provfile := path + ".prov"
	if _, err := os.Stat(provfile); err != nil {
		return nil, errors.Wrapf(err, "could not load provenance file %s", provfile)
	}

	sig, err := provenance.NewFromKeyring(keyring, "")
	if err != nil {
		return nil, errors.Wrap(err, "failed to load keyring")
	}
	return sig.Verify(path, provfile)
}

// isTar tests whether the given file is a tar file.
//
// Currently, this simply checks extension, since a subsequent function will
// untar the file and validate its binary format.
func isTar(filename string) bool {
	return strings.ToLower(filepath.Ext(filename)) == ".tgz"
}

func pickChartRepositoryConfigByName(name string, cfgs []*repo.Entry) (*repo.Entry, error) {
	for _, rc := range cfgs {
		if rc.Name == name {
			if rc.URL == "" {
				return nil, errors.Errorf("no URL found for repository %s", name)
			}
			return rc, nil
		}
	}
	return nil, errors.Errorf("repo %s not found", name)
}

// scanReposForURL scans all repos to find which repo contains the given URL.
//
// This will attempt to find the given URL in all of the known repositories files.
//
// If the URL is found, this will return the repo entry that contained that URL.
//
// If all of the repos are checked, but the URL is not found, an ErrNoOwnerRepo
// error is returned.
//
// Other errors may be returned when repositories cannot be loaded or searched.
//
// Technically, the fact that a URL is not found in a repo is not a failure indication.
// Charts are not required to be included in an index before they are valid. So
// be mindful of this case.
//
// The same URL can technically exist in two or more repositories. This algorithm
// will return the first one it finds. Order is determined by the order of repositories
// in the repositories.yaml file.
func (c *ChartDownloader) scanReposForURL(u string, rf *repo.File) (*repo.Entry, error) {
	// FIXME: This is far from optimal. Larger installations and index files will
	// incur a performance hit for this type of scanning.
	for _, rc := range rf.Repositories {
		r, err := repo.NewChartRepository(rc, c.Getters)
		if err != nil {
			return nil, err
		}

		i, err := repo.LoadIndexFile(c.HelmHome.CacheIndex(r.Config.Name))
		if err != nil {
			return nil, errors.Wrap(err, "no cached repo found. (try 'helm repo update')")
		}

		for _, entry := range i.Entries {
			for _, ver := range entry {
				for _, dl := range ver.URLs {
					if urlutil.Equal(u, dl) {
						return rc, nil
					}
				}
			}
		}
	}
	// This means that there is no repo file for the given URL.
	return nil, ErrNoOwnerRepo
}
