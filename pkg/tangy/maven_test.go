package tangy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRepositoryHref(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		href    string
		want    string
		wantErr bool
	}{
		{
			name: "valid maven repository href",
			href: "/api/pulp/default/api/v3/repositories/maven/maven/018c1c95-4281-76eb-b277-842cbad524f4/",
			want: "018c1c95-4281-76eb-b277-842cbad524f4",
		},
		{
			name: "valid href without trailing slash",
			href: "/api/pulp/default/api/v3/repositories/maven/maven/018c1c95-4281-76eb-b277-842cbad524f4",
			want: "018c1c95-4281-76eb-b277-842cbad524f4",
		},
		{
			name:    "invalid href",
			href:    "/api/pulp/default/api/v3/repositories/maven/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseRepositoryHref(tt.href)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStripMavenReleaseVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		want    string
	}{
		// Legacy format tests
		{
			name:    "release suffix in version",
			version: "5.3.18.rhlw-00003",
			want:    "5.3.18",
		},
		{
			name:    "plain version",
			version: "1.2.3",
			want:    "1.2.3",
		},
		{
			name:    "final qualifier version",
			version: "4.1.114.Final",
			want:    "4.1.114.Final",
		},
		{
			name:    "empty version",
			version: "",
			want:    "",
		},
		// New format tests
		{
			name:    "baseline only",
			version: "1.2.3-rhlw.00003",
			want:    "1.2.3",
		},
		{
			name:    "baseline with novel",
			version: "1.2.3-rhlw.00003.n00001",
			want:    "1.2.3",
		},
		{
			name:    "baseline with hotfix",
			version: "1.2.3-rhlw.00003.hf00001",
			want:    "1.2.3",
		},
		{
			name:    "baseline with novel and hotfix",
			version: "1.2.3-rhlw.00003.n00001.hf00002",
			want:    "1.2.3",
		},
		{
			name:    "snapshot version not stripped",
			version: "1.2.3-SNAPSHOT",
			want:    "1.2.3-SNAPSHOT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, stripMavenReleaseVersion(tt.version))
		})
	}
}

func TestMavenRepositoryMetricsCounting(t *testing.T) {
	t.Parallel()

	type artifact struct {
		group    string
		artifact string
		version  string
	}

	countMetrics := func(artifacts []artifact) (packageCount, buildCount, versionCount int) {
		packages := make(map[[2]string]struct{})
		builds := make(map[[3]string]struct{})
		versions := make(map[[3]string]struct{})

		for _, a := range artifacts {
			packages[[2]string{a.group, a.artifact}] = struct{}{}
			builds[[3]string{a.group, a.artifact, a.version}] = struct{}{}
			baseVersion := stripMavenReleaseVersion(a.version)
			versions[[3]string{a.group, a.artifact, baseVersion}] = struct{}{}
		}

		return len(packages), len(builds), len(versions)
	}

	springCoreVersions := []string{
		"5.3.18.rhlw-00003",
		"5.3.18.rhlw-00004",
		"5.3.18.rhlw-00005",
		"5.3.18.rhlw-00006",
		"5.3.18.rhlw-00007",
	}
	springCoreArtifacts := make([]artifact, 0, len(springCoreVersions)*3)
	for _, version := range springCoreVersions {
		for range 3 {
			springCoreArtifacts = append(springCoreArtifacts, artifact{
				group:    "org.springframework",
				artifact: "spring-core",
				version:  version,
			})
		}
	}

	nettyVersions := []string{
		"4.1.114.Final",
		"4.1.127.Final",
		"4.1.128.Final",
		"4.1.130.Final",
		"4.1.133.Final",
	}
	nettyArtifacts := make([]artifact, 0, len(nettyVersions)*5)
	for _, version := range nettyVersions {
		for range 5 {
			nettyArtifacts = append(nettyArtifacts, artifact{
				group:    "io.netty",
				artifact: "netty-codec",
				version:  version,
			})
		}
	}

	packageCount, buildCount, versionCount := countMetrics(springCoreArtifacts)
	assert.Equal(t, 1, packageCount)
	assert.Equal(t, 5, buildCount)
	assert.Equal(t, 1, versionCount)

	packageCount, buildCount, versionCount = countMetrics(nettyArtifacts)
	assert.Equal(t, 1, packageCount)
	assert.Equal(t, 5, buildCount)
	assert.Equal(t, 5, versionCount)
}

func TestExtractRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		want     string
	}{
		// Legacy format tests
		{
			name:     "release suffix in pom filename",
			filename: "smallrye-mutiny-vertx-core-3.16.0.rhlw-3002.pom",
			want:     "rhlw-3002",
		},
		{
			name:     "plain pom filename",
			filename: "junit-4.13.2.pom",
			want:     "",
		},
		{
			name:     "empty filename",
			filename: "",
			want:     "",
		},
		// New format tests
		{
			name:     "baseline only",
			filename: "artifact-1.2.3-rhlw.00003.pom",
			want:     "rhlw.00003",
		},
		{
			name:     "baseline with novel",
			filename: "artifact-1.2.3-rhlw.00003.n00001.pom",
			want:     "rhlw.00003.n00001",
		},
		{
			name:     "baseline with hotfix",
			filename: "artifact-1.2.3-rhlw.00003.hf00001.pom",
			want:     "rhlw.00003.hf00001",
		},
		{
			name:     "baseline with novel and hotfix",
			filename: "artifact-1.2.3-rhlw.00003.n00001.hf00002.pom",
			want:     "rhlw.00003.n00001.hf00002",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, extractRelease(tt.filename))
		})
	}
}

func TestMockTangyMavenPackageList(t *testing.T) {
	t.Parallel()

	mockTangy := NewMockTangy(t)
	ctx := context.Background()
	repoHref := "/api/pulp/default/api/v3/repositories/maven/maven/018c1c95-4281-76eb-b277-842cbad524f4/"
	pageOpts := PageOptions{Offset: 0, Limit: 10}
	filterOpts := MavenPackageListFilters{Search: "junit"}

	expected := MavenPackageListResponse{
		Results: []MavenPackageListItem{
			{
				GroupID:    "junit",
				ArtifactID: "junit",
				Versions:   []string{"4.13.2"},
				LatestReleases: []MavenReleaseInfo{
					{Version: "4.13.2", Release: "", CreatedAt: "2024-01-01T12:00:00Z"},
				},
			},
		},
		Total:  1,
		Limit:  10,
		Offset: 0,
	}

	mockTangy.On("MavenPackageList", ctx, repoHref, filterOpts, pageOpts).Return(expected, nil)

	got, err := mockTangy.MavenPackageList(ctx, repoHref, filterOpts, pageOpts)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestMockTangyMavenVersionsList(t *testing.T) {
	t.Parallel()

	mockTangy := NewMockTangy(t)
	ctx := context.Background()
	repoHref := "/api/pulp/default/api/v3/repositories/maven/maven/018c1c95-4281-76eb-b277-842cbad524f4/"
	pageOpts := PageOptions{Offset: 0, Limit: 10}

	expected := MavenVersionsResponse{
		Results: []MavenVersionsItem{
			{
				GroupID:    "junit",
				ArtifactID: "junit",
				Version:    "4.13.2",
				Builds: []MavenBuildInfo{
					{
						Version:   "4.13.2",
						Release:   "",
						Filename:  "junit-4.13.2.pom",
						CreatedAt: "2024-01-01T12:00:00Z",
					},
				},
			},
		},
		Total:  1,
		Limit:  10,
		Offset: 0,
	}

	mockTangy.On("MavenVersionsList", ctx, repoHref, "junit", "junit", "4.13.2", pageOpts).Return(expected, nil)

	got, err := mockTangy.MavenVersionsList(ctx, repoHref, "junit", "junit", "4.13.2", pageOpts)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestMockTangyMavenRepositoryMetrics(t *testing.T) {
	t.Parallel()

	mockTangy := NewMockTangy(t)
	ctx := context.Background()
	repoHref := "/api/pulp/default/api/v3/repositories/maven/maven/018c1c95-4281-76eb-b277-842cbad524f4/"

	expected := MavenRepositoryMetrics{
		PackageCount: 1,
		BuildCount:   1,
		VersionCount: 1,
	}

	mockTangy.On("MavenRepositoryMetrics", ctx, repoHref).Return(expected, nil)

	got, err := mockTangy.MavenRepositoryMetrics(ctx, repoHref)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}
