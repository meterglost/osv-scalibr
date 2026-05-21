// Copyright 2026 Google LLC
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

// Package mavenutil provides utilities for merging Maven pom/xml.
package mavenutil

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"deps.dev/util/maven"
	"deps.dev/util/resolve"
	"deps.dev/util/semver"
	"github.com/google/osv-scalibr/clients/datasource"
	"github.com/google/osv-scalibr/extractor/filesystem"
	"github.com/google/osv-scalibr/log"
)

// Origin of the dependencies.
const (
	OriginManagement = "management"
	OriginParent     = "parent"
	OriginPlugin     = "plugin"
	OriginProfile    = "profile"
)

// MaxParent sets a limit on the number of parents to avoid indefinite loop.
const MaxParent = 100

// Options for merging parent data.
//   - Input is the scan input for the current project.
//   - Client is the Maven registry API client for fetching remote pom.xml.
//   - AllowLocal indicates whether parsing local parent pom.xml is allowed.
//   - InitialParentIndex indicates the index of the current parent project, which is
//     used to check if the packaging has to be `pom`.
type Options struct {
	Input  *filesystem.ScanInput
	Client *datasource.MavenRegistryAPIClient

	AddRegistry        bool
	AllowLocal         bool
	InitialParentIndex int
}

// MergeParents parses local accessible parent pom.xml or fetches it from
// upstream, merges into root project, then interpolate the properties.
//   - current holds the current parent project to merge.
//   - result holds the Maven project to merge into, this is modified in place.
//   - opts holds the options for merging parent data.
func MergeParents(ctx context.Context, current maven.Parent, result *maven.Project, opts Options) error {
	currentPath := ""
	if opts.Input != nil {
		currentPath = opts.Input.Path
	}

	allowLocal := opts.AllowLocal
	visited := make(map[maven.ProjectKey]struct{}, MaxParent)
	for n := opts.InitialParentIndex; n < MaxParent; n++ {
		if current.GroupID == "" || current.ArtifactID == "" || current.Version == "" {
			break
		}
		if _, ok := visited[current.ProjectKey]; ok {
			// A cycle of parents is detected
			return errors.New("a cycle of parents is detected")
		}
		visited[current.ProjectKey] = struct{}{}

		var proj maven.Project
		parentFoundLocally := false
		if allowLocal {
			var parentPath string
			var err error
			parentFoundLocally, parentPath, err = loadParentLocal(opts.Input, current, currentPath, &proj)
			if err != nil {
				return fmt.Errorf("failed to load parent at %s: %w", currentPath, err)
			}
			if parentPath != "" {
				currentPath = parentPath
			}
		}
		if !parentFoundLocally {
			// Once we fetch a parent pom.xml from upstream, we should not
			// allow parsing parent pom.xml locally anymore.
			allowLocal = false
			var err error
			proj, err = loadParentRemote(ctx, opts.Client, current, n)
			if err != nil {
				return fmt.Errorf("failed to load parent from remote: %w", err)
			}
		}
		// Use an empty JDK string and ActivationOS here to merge the default profiles.
		if err := result.MergeProfiles("", maven.ActivationOS{}); err != nil {
			return fmt.Errorf("failed to merge default profiles: %w", err)
		}
		// Interpolate the repositories in the project to get rid of the placeholders in URLs.
		if err := proj.InterpolateRepositories(); err != nil {
			return fmt.Errorf("failed to interpolate repositories: %w", err)
		}
		if opts.Client != nil && opts.AddRegistry && len(proj.Repositories) > 0 {
			for _, repo := range proj.Repositories {
				if err := opts.Client.AddRegistry(ctx, datasource.MavenRegistry{
					URL:              string(repo.URL),
					ID:               string(repo.ID),
					ReleasesEnabled:  repo.Releases.Enabled.Boolean(),
					SnapshotsEnabled: repo.Snapshots.Enabled.Boolean(),
				}); err != nil {
					return fmt.Errorf("failed to add registry %s: %w", repo.URL, err)
				}
			}
		}
		result.MergeParent(proj)
		current = proj.Parent
	}
	// Interpolate the project to resolve the properties.
	return result.Interpolate()
}

// loadParentLocal loads a parent Maven project from local file system
// and returns whether parent is found locally as well as parent path.
func loadParentLocal(input *filesystem.ScanInput, parent maven.Parent, path string, result *maven.Project) (bool, string, error) {
	parentPath := ParentPOMPath(input, path, string(parent.RelativePath))
	if parentPath == "" {
		return false, "", nil
	}
	f, err := input.FS.Open(parentPath)
	if err != nil {
		return false, "", fmt.Errorf("failed to open parent file %s: %w", parentPath, err)
	}
	err = datasource.NewMavenDecoder(f).Decode(result)
	if closeErr := f.Close(); closeErr != nil {
		return false, "", fmt.Errorf("failed to close file: %w", err)
	}
	if err != nil {
		return false, "", fmt.Errorf("failed to unmarshal project: %w", err)
	}
	if ProjectKey(*result) != parent.ProjectKey || result.Packaging != "pom" {
		// Only mark parent as found when the identifiers and packaging are expected.
		return false, "", nil
	}
	return true, parentPath, nil
}

// loadParentRemote loads a parent from remote registry.
func loadParentRemote(ctx context.Context, mavenClient *datasource.MavenRegistryAPIClient, parent maven.Parent, parentIndex int) (maven.Project, error) {
	if mavenClient == nil {
		// The client is not available, so return an empty project.
		return maven.Project{}, nil
	}

	proj, err := mavenClient.GetProject(ctx, string(parent.GroupID), string(parent.ArtifactID), string(parent.Version))
	if err != nil {
		return maven.Project{}, fmt.Errorf("failed to get Maven project %s:%s:%s: %w", parent.GroupID, parent.ArtifactID, parent.Version, err)
	}
	if parentIndex > 0 && proj.Packaging != "pom" {
		// A parent project should only be of "pom" packaging type.
		return maven.Project{}, fmt.Errorf("invalid packaging for parent project %s", proj.Packaging)
	}
	if ProjectKey(proj) != parent.ProjectKey {
		// The identifiers in parent does not match what we want.
		return maven.Project{}, fmt.Errorf("parent identifiers mismatch: %v, expect %v", proj.ProjectKey, parent.ProjectKey)
	}
	return proj, nil
}

// ProjectKey returns a project key with empty groupId/version
// filled by corresponding fields in parent.
func ProjectKey(proj maven.Project) maven.ProjectKey {
	if proj.GroupID == "" {
		proj.GroupID = proj.Parent.GroupID
	}
	if proj.Version == "" {
		proj.Version = proj.Parent.Version
	}

	return proj.ProjectKey
}

// ParentPOMPath returns the path of a parent pom.xml.
// Maven looks for the parent POM first in 'relativePath', then
// the local repository '../pom.xml', and lastly in the remote repo.
// An empty string is returned if failed to resolve the parent path.
func ParentPOMPath(input *filesystem.ScanInput, currentPath, relativePath string) string {
	if relativePath == "" {
		relativePath = "../pom.xml"
	}

	path := filepath.ToSlash(filepath.Join(filepath.Dir(currentPath), relativePath))
	if info, err := input.FS.Stat(path); err == nil {
		if !info.IsDir() {
			return path
		}
		// Current path is a directory, so look for pom.xml in the directory.
		path = filepath.ToSlash(filepath.Join(path, "pom.xml"))
		if _, err := input.FS.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// WorkspaceModule stores the path and version of a Maven module in the workspace.
type WorkspaceModule struct {
	Path    string
	Version string
}

// WorkspaceIndex maps Maven coordinates (groupId:artifactId) to their
// local path and version. This enables O(1) lookups when resolving
// workspace-local BOMs during dependency processing.
type WorkspaceIndex map[string]WorkspaceModule

// pomWithModules is used to parse a pom.xml including its <modules> section.
type pomWithModules struct {
	maven.Project
	Modules []string `xml:"modules>module"`
}

// BuildWorkspaceIndex builds an index of Maven modules by tracing the reactor
// structure from the given pom.xml paths. Instead of walking the entire filesystem,
// it follows <modules> declarations and parent references to discover all related
// modules. This should be called once per scan and reused for all
// GetDependencyManagement calls.
func BuildWorkspaceIndex(fsys filesystem.ScanInput, pomPaths []string) WorkspaceIndex {
	if fsys.FS == nil || len(pomPaths) == 0 {
		return nil
	}

	idx := make(WorkspaceIndex)
	visited := make(map[string]bool)

	// indexPom reads a pom.xml and adds it to the index, then recursively
	// indexes any <modules> declared in it.
	var indexPom func(pomPath string)
	indexPom = func(pomPath string) {
		if visited[pomPath] {
			return
		}
		visited[pomPath] = true

		f, err := fsys.FS.Open(pomPath)
		if err != nil {
			return
		}
		var proj pomWithModules
		decodeErr := datasource.NewMavenDecoder(f).Decode(&proj)
		f.Close()
		if decodeErr != nil {
			return
		}

		key := ProjectKey(proj.Project)
		if key.GroupID != "" && key.ArtifactID != "" {
			idx[key.Name()] = WorkspaceModule{
				Path:    pomPath,
				Version: string(key.Version),
			}
		}

		// Trace child modules.
		parentDir := filepath.Dir(pomPath)
		for _, mod := range proj.Modules {
			childPom := filepath.Join(parentDir, mod, "pom.xml")
			childPom = filepath.ToSlash(childPom)
			indexPom(childPom)
		}

		// Trace parent (walk up to root reactor).
		if proj.Parent.ArtifactID != "" {
			relPath := string(proj.Parent.RelativePath)
			if relPath == "" {
				relPath = "../pom.xml"
			}
			parentPom := filepath.Join(parentDir, relPath)
			parentPom = filepath.ToSlash(filepath.Clean(parentPom))
			indexPom(parentPom)
		}
	}

	for _, p := range pomPaths {
		indexPom(p)
	}

	log.Infof("built workspace index with %d Maven modules from %d starting POMs", len(idx), len(pomPaths))
	return idx
}

// LoadLocalProject loads and parses a pom.xml from the workspace filesystem.
// Note: Interpolate() is NOT called here — callers should merge parents first,
// then interpolate, to avoid dropping dependencies with parent-defined properties.
func LoadLocalProject(input *filesystem.ScanInput, path string) (maven.Project, error) {
	f, err := input.FS.Open(path)
	if err != nil {
		return maven.Project{}, fmt.Errorf("failed to open local project %s: %w", path, err)
	}
	var proj maven.Project
	decodeErr := datasource.NewMavenDecoder(f).Decode(&proj)
	f.Close()
	if decodeErr != nil {
		return maven.Project{}, fmt.Errorf("failed to decode local project %s: %w", path, decodeErr)
	}
	return proj, nil
}

// GetDependencyManagement returns managed dependencies in the specified Maven project.
// If wsIndex and input are provided, it first attempts to resolve the project from
// the local workspace before falling back to fetching from remote Maven registries.
// Results are cached via bomCache to avoid redundant remote fetches. Concurrent
// requests for the same BOM are coalesced automatically.
func GetDependencyManagement(ctx context.Context, client *datasource.MavenRegistryAPIClient, groupID, artifactID, version maven.String, wsIndex WorkspaceIndex, input *filesystem.ScanInput, bomCache *datasource.RequestCache[string, maven.DependencyManagement]) (maven.DependencyManagement, error) {
	cacheKey := fmt.Sprintf("%s:%s:%s", groupID, artifactID, version)

	if bomCache == nil {
		return getDependencyManagementUncached(ctx, client, groupID, artifactID, version, wsIndex, input)
	}

	return bomCache.Get(cacheKey, func() (maven.DependencyManagement, error) {
		return getDependencyManagementUncached(ctx, client, groupID, artifactID, version, wsIndex, input)
	})
}

// getDependencyManagementUncached performs the actual resolution without caching.
func getDependencyManagementUncached(ctx context.Context, client *datasource.MavenRegistryAPIClient, groupID, artifactID, version maven.String, wsIndex WorkspaceIndex, input *filesystem.ScanInput) (maven.DependencyManagement, error) {
	// Try local workspace resolution first.
	if wsIndex != nil && input != nil {
		name := maven.ProjectKey{GroupID: groupID, ArtifactID: artifactID}.Name()
		if mod, ok := wsIndex[name]; ok {
			result, err := LoadLocalProject(input, mod.Path)
			if err != nil {
				log.Warnf("failed to load local project %s: %v, falling back to remote", mod.Path, err)
			} else {
				log.Debugf("resolved %s:%s locally from %s", groupID, artifactID, mod.Path)
				// Merge parents to get inherited dependency management.
				if err := MergeParents(ctx, result.Parent, &result, Options{
					Input:              input,
					Client:             client,
					AddRegistry:        false,
					AllowLocal:         true,
					InitialParentIndex: 1,
				}); err != nil {
					log.Warnf("failed to merge parents for local %s: %v, falling back to remote", mod.Path, err)
				} else {
					return result.DependencyManagement, nil
				}
			}
		}
	}

	// Fall back to fetching from remote.
	root := maven.Parent{ProjectKey: maven.ProjectKey{GroupID: groupID, ArtifactID: artifactID, Version: version}}
	var result maven.Project
	// To get dependency management from another project, we need the
	// project with parents merged, so we call MergeParents by passing
	// an empty project.
	if err := MergeParents(ctx, root, &result, Options{
		Client:             client,
		AddRegistry:        false,
		AllowLocal:         false,
		InitialParentIndex: 0,
	}); err != nil {
		return maven.DependencyManagement{}, err
	}

	return result.DependencyManagement, nil
}

// CompareVersions compares two Maven semver versions with special behaviour for specific packages,
// producing more desirable ordering using non-standard comparison.
func CompareVersions(vk resolve.VersionKey, a *semver.Version, b *semver.Version) int {
	if a == nil || b == nil {
		if a == nil {
			return -1
		}

		return 1
	}

	if vk.Name == "com.google.guava:guava" {
		// com.google.guava:guava has 'flavors' with versions ending with -jre or -android.
		// https://github.com/google/guava/wiki/ReleasePolicy#flavors
		// To preserve the flavor in updates, we make the opposite flavor considered the earliest versions.

		// Old versions have '22.0' and '22.0-android', and even older version don't have any flavors.
		// Only check for the android flavor, and assume its jre otherwise.
		wantAndroid := strings.HasSuffix(vk.Version, "-android")

		aIsAndroid := strings.HasSuffix(a.String(), "-android")
		bIsAndroid := strings.HasSuffix(b.String(), "-android")

		if aIsAndroid == bIsAndroid {
			return a.Compare(b)
		}

		if aIsAndroid == wantAndroid {
			return 1
		}

		return -1
	}

	// Old versions of apache commons-* libraries (commons-io:commons-io, commons-math:commons-math, etc.)
	// used date-based versions (e.g. 20040118.003354), which naturally sort after the more recent semver versions.
	// We manually force the date versions to come before the others to prevent downgrades.
	if strings.HasPrefix(vk.Name, "commons-") {
		// All date-based versions of these packages seem to be in the years 2002-2005.
		// It's extremely unlikely we'd see any versions dated before 1999 or after 2010.
		// It's also unlikely we'd see any major versions of these packages reach up to 200.0.0.
		// Checking if the version starts with "200" should therefore be sufficient to determine if it's a year.
		aCal := strings.HasPrefix(a.String(), "200")
		bCal := strings.HasPrefix(b.String(), "200")

		if aCal == bCal {
			return a.Compare(b)
		}

		if aCal {
			return -1
		}

		return 1
	}

	return a.Compare(b)
}

// IsPrerelease returns whether the given version is a prerelease version.
// There is a special handling for com.google.guava:guava, which has 'flavors' with versions ending
// with '-jre' or '-android'. These versions are not considered as prerelease versions.
func IsPrerelease(ver *semver.Version, vk resolve.VersionKey) bool {
	if vk.Name == "com.google.guava:guava" {
		return false
	}
	return ver.IsPrerelease()
}
