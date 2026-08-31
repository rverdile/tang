package tangy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type MavenReleaseInfo struct {
	Version   string `json:"version"`
	Release   string `json:"release"`
	CreatedAt string `json:"created_at"`
}

type MavenPackageListItem struct {
	GroupID        string             `json:"group_id"`
	ArtifactID     string             `json:"artifact_id"`
	Versions       []string           `json:"versions"`
	LatestReleases []MavenReleaseInfo `json:"latest_releases"`
}

type MavenPackageListFilters struct {
	Search string
}

type MavenPackageListResponse struct {
	Results []MavenPackageListItem `json:"results"`
	Total   int                    `json:"total"`
	Limit   int                    `json:"limit"`
	Offset  int                    `json:"offset"`
}

type MavenBuildInfo struct {
	Version   string `json:"version"`
	Release   string `json:"release"`
	Filename  string `json:"filename"`
	CreatedAt string `json:"created_at"`
}

type MavenVersionsItem struct {
	GroupID    string           `json:"group_id"`
	ArtifactID string           `json:"artifact_id"`
	Version    string           `json:"version"`
	Builds     []MavenBuildInfo `json:"builds"`
}

type MavenVersionsResponse struct {
	Results []MavenVersionsItem `json:"results"`
	Total   int                 `json:"total"`
	Limit   int                 `json:"limit"`
	Offset  int                 `json:"offset"`
}

type MavenRepositoryMetrics struct {
	PackageCount int `json:"package_count"`
	BuildCount   int `json:"build_count"`
	VersionCount int `json:"version_count"`
}

// MavenPackageList lists Maven packages from the latest version of a repository, grouped by group_id and artifact_id
// Only includes artifacts with .pom files
func (t *tangyImpl) MavenPackageList(ctx context.Context, repositoryHref string, filterOpts MavenPackageListFilters, pageOpts PageOptions) (MavenPackageListResponse, error) {
	if repositoryHref == "" {
		return MavenPackageListResponse{}, nil
	}

	conn, err := t.pool.Acquire(ctx)
	if err != nil {
		return MavenPackageListResponse{}, err
	}
	defer conn.Release()

	if pageOpts.Limit == 0 {
		pageOpts.Limit = DefaultLimit
	}

	// Parse repository UUID from href
	repoUUID, err := parseRepositoryHref(repositoryHref)
	if err != nil {
		return MavenPackageListResponse{}, fmt.Errorf("error parsing repository href: %w", err)
	}

	// Get the latest version for this repository
	latestVersion, err := getLatestRepositoryVersion(ctx, conn, repoUUID)
	if err != nil {
		return MavenPackageListResponse{}, fmt.Errorf("error getting latest repository version: %w", err)
	}

	repoVerMap := []ParsedRepoVersion{{
		RepositoryUUID: repoUUID,
		Version:        latestVersion,
	}}

	args := pgx.NamedArgs{}
	pomFilter := ` AND rp.filename LIKE '%.pom'`
	searchFilter := ""
	if filterOpts.Search != "" {
		args["searchFilter"] = filterOpts.Search
		searchFilter = ` AND (rp.group_id ILIKE CONCAT('%', @searchFilter::text, '%')
			OR rp.artifact_id ILIKE CONCAT(@searchFilter::text, '%'))`
	}
	innerUnion, err := contentIdsInVersions(ctx, conn, repoVerMap, &args)
	if err != nil {
		return MavenPackageListResponse{}, err
	}

	artifactFilters := pomFilter + searchFilter

	// Count query for total grouped packages
	// Note: using 'rp' alias as required by contentIdsInVersions function
	countQuery := `
		SELECT COUNT(DISTINCT (rp.group_id, rp.artifact_id))
		FROM maven_mavenartifact rp
	` + innerUnion + artifactFilters

	var countTotal int
	err = conn.QueryRow(ctx, countQuery, args).Scan(&countTotal)
	if err != nil {
		return MavenPackageListResponse{}, err
	}

	// Main query using SQL aggregation and pagination
	// This query groups by group_id/artifact_id, collects versions, and finds latest release per version
	args["limit"] = pageOpts.Limit
	args["offset"] = pageOpts.Offset

	query := `
		WITH package_versions AS (
			SELECT
				rp.group_id,
				rp.artifact_id,
				regexp_replace(rp.version, '` + mavenReleaseVersionSuffixPattern + `', '') as base_version,
				rp.filename,
				cc.pulp_created,
				ROW_NUMBER() OVER (PARTITION BY rp.group_id, rp.artifact_id, regexp_replace(rp.version, '` + mavenReleaseVersionSuffixPattern + `', '') ORDER BY cc.pulp_created DESC) as rn
			FROM maven_mavenartifact rp
			INNER JOIN core_content cc ON rp.content_ptr_id = cc.pulp_id
		` + innerUnion + artifactFilters + `
		),
		latest_per_version AS (
			SELECT
				group_id,
				artifact_id,
				base_version,
				filename,
				pulp_created
			FROM package_versions
			WHERE rn = 1
		),
		packages AS (
			SELECT
				group_id,
				artifact_id,
				ARRAY_AGG(DISTINCT base_version ORDER BY base_version) as versions
			FROM latest_per_version
			GROUP BY group_id, artifact_id
			ORDER BY group_id, artifact_id
			LIMIT @limit OFFSET @offset
		)
		SELECT
			p.group_id,
			p.artifact_id,
			p.versions,
			COALESCE(
				JSON_AGG(
					JSON_BUILD_OBJECT(
						'version', lpv.base_version,
						'release', '',
						'filename', lpv.filename,
						'created_at', lpv.pulp_created
					) ORDER BY lpv.base_version
				) FILTER (WHERE lpv.base_version IS NOT NULL),
				'[]'::json
			) as latest_releases_json
		FROM packages p
		LEFT JOIN latest_per_version lpv ON p.group_id = lpv.group_id AND p.artifact_id = lpv.artifact_id
		GROUP BY p.group_id, p.artifact_id, p.versions
		ORDER BY p.group_id, p.artifact_id`

	rows, err := conn.Query(ctx, query, args)
	if err != nil {
		return MavenPackageListResponse{}, err
	}

	type queryResult struct {
		GroupID            string
		ArtifactID         string
		Versions           []string
		LatestReleasesJSON []byte
	}

	queryResults, err := pgx.CollectRows(rows, pgx.RowToStructByName[queryResult])
	if err != nil {
		return MavenPackageListResponse{}, err
	}

	// Convert query results to response format
	results := make([]MavenPackageListItem, 0, len(queryResults))
	for _, qr := range queryResults {
		var latestReleases []struct {
			Version   string    `json:"version"`
			Release   string    `json:"release"`
			Filename  string    `json:"filename"`
			CreatedAt time.Time `json:"created_at"`
		}

		if err := json.Unmarshal(qr.LatestReleasesJSON, &latestReleases); err != nil {
			return MavenPackageListResponse{}, fmt.Errorf("failed to parse latest_releases: %w", err)
		}

		// Extract release info and format timestamps
		releaseInfos := make([]MavenReleaseInfo, 0, len(latestReleases))
		for _, lr := range latestReleases {
			releaseInfos = append(releaseInfos, MavenReleaseInfo{
				Version:   lr.Version,
				Release:   extractRelease(lr.Filename),
				CreatedAt: lr.CreatedAt.Format(time.RFC3339),
			})
		}

		results = append(results, MavenPackageListItem{
			GroupID:        qr.GroupID,
			ArtifactID:     qr.ArtifactID,
			Versions:       qr.Versions,
			LatestReleases: releaseInfos,
		})
	}

	return MavenPackageListResponse{
		Results: results,
		Total:   countTotal,
		Limit:   pageOpts.Limit,
		Offset:  pageOpts.Offset,
	}, nil
}

// OLD format: .rhlw-00003 (dot before "rhlw", hyphen before digits)
const mavenLegacyReleasePattern = `[a-zA-Z]+-\d+`

// NEW format: -rhlw.00003[.n00001][.hf00001] (hyphen before "rhlw", dot separators)
// Matches: -rhlw.NNNNN or -rhlw.NNNNN.nNNNNN or -rhlw.NNNNN.hfNNNNN or -rhlw.NNNNN.nNNNNN.hfNNNNN
const mavenNewReleasePattern = `rhlw\.\d+(?:\.(?:n|hf)\d+)*`

// Combined pattern for version suffix: matches either old OR new format
// OLD: .rhlw-00003 or NEW: -rhlw.00003.n00001
const mavenReleaseVersionSuffixPattern = `(?:\.` + mavenLegacyReleasePattern + `|-` + mavenNewReleasePattern + `)$`

// Combined pattern for filename extraction
// OLD: .rhlw-00003.pom or NEW: -rhlw.00003.n00001.pom
const mavenReleaseFilenamePattern = `(\.` + mavenLegacyReleasePattern + `|-` + mavenNewReleasePattern + `)\.pom$`

var mavenReleaseVersionSuffixRegexp = regexp.MustCompile(mavenReleaseVersionSuffixPattern)

var mavenReleaseFilenameRegexp = regexp.MustCompile(mavenReleaseFilenamePattern)

// stripMavenReleaseVersion removes a trailing release qualifier from a Maven version string.
// Example: 5.3.18.rhlw-00003 -> 5.3.18
func stripMavenReleaseVersion(version string) string {
	return mavenReleaseVersionSuffixRegexp.ReplaceAllString(version, "")
}

// extractRelease extracts the release version from a filename
// Examples:
//   - smallrye-mutiny-vertx-core-3.16.0.rhlw-3002.pom -> rhlw-3002 (OLD)
//   - artifact-1.2.3-rhlw.00003.n00001.pom -> rhlw.00003.n00001 (NEW)
func extractRelease(filename string) string {
	matches := mavenReleaseFilenameRegexp.FindStringSubmatch(filename)
	if len(matches) > 1 {
		// Remove leading dot or hyphen from captured group
		release := matches[1]
		if len(release) > 0 && (release[0] == '.' || release[0] == '-') {
			return release[1:]
		}
		return release
	}
	return ""
}

// parseRepositoryHref extracts the repository UUID from a repository href
// Example: /api/pulp/default/api/v3/repositories/maven/maven/018c1c95-4281-76eb-b277-842cbad524f4/
func parseRepositoryHref(href string) (string, error) {
	parts := strings.Split(href, "/")
	// Filter out empty parts
	var nonEmptyParts []string
	for _, part := range parts {
		if part != "" {
			nonEmptyParts = append(nonEmptyParts, part)
		}
	}

	// Expected format: api/pulp/{domain}/api/v3/repositories/maven/maven/{uuid}
	// So we need at least 8 parts
	if len(nonEmptyParts) < 8 {
		return "", fmt.Errorf("invalid repository href format: %s", href)
	}

	// The UUID should be the last part (or second to last if there's a trailing slash)
	repoUUID := nonEmptyParts[len(nonEmptyParts)-1]

	return repoUUID, nil
}

// getLatestRepositoryVersion gets the highest version number for a repository
func getLatestRepositoryVersion(ctx context.Context, conn *pgxpool.Conn, repoUUID string) (int, error) {
	query := `
		SELECT MAX(number)
		FROM core_repositoryversion
		WHERE repository_id = $1 AND complete = true
	`

	var latestVersion int
	err := conn.QueryRow(ctx, query, repoUUID).Scan(&latestVersion)
	if err != nil {
		return 0, fmt.Errorf("failed to get latest version for repository %s: %w", repoUUID, err)
	}

	return latestVersion, nil
}

// MavenVersionsList lists all Maven artifacts (builds), optionally filtered by group_id, artifact_id, and version
// from the latest version of a repository
func (t *tangyImpl) MavenVersionsList(ctx context.Context, repositoryHref, groupID, artifactID, version string, pageOpts PageOptions) (MavenVersionsResponse, error) {
	if repositoryHref == "" {
		return MavenVersionsResponse{}, nil
	}

	conn, err := t.pool.Acquire(ctx)
	if err != nil {
		return MavenVersionsResponse{}, err
	}
	defer conn.Release()

	if pageOpts.Limit == 0 {
		pageOpts.Limit = DefaultLimit
	}

	// Parse repository UUID from href
	repoUUID, err := parseRepositoryHref(repositoryHref)
	if err != nil {
		return MavenVersionsResponse{}, fmt.Errorf("error parsing repository href: %w", err)
	}

	// Get the latest version for this repository
	latestVersion, err := getLatestRepositoryVersion(ctx, conn, repoUUID)
	if err != nil {
		return MavenVersionsResponse{}, fmt.Errorf("error getting latest repository version: %w", err)
	}

	repoVerMap := []ParsedRepoVersion{{
		RepositoryUUID: repoUUID,
		Version:        latestVersion,
	}}

	args := pgx.NamedArgs{}

	// Build WHERE clause conditionally based on provided parameters
	var whereClause string
	if groupID != "" {
		args["group_id"] = groupID
		whereClause += "\n\t\tAND rp.group_id = @group_id"
	}
	if artifactID != "" {
		args["artifact_id"] = artifactID
		whereClause += "\n\t\tAND rp.artifact_id = @artifact_id"
	}
	if version != "" {
		args["version"] = version
		whereClause += "\n\t\tAND regexp_replace(rp.version, '" + strings.ReplaceAll(mavenReleaseVersionSuffixPattern, `\`, `\\`) + "', '') = @version"
	}

	innerUnion, err := contentIdsInVersions(ctx, conn, repoVerMap, &args)
	if err != nil {
		return MavenVersionsResponse{}, err
	}

	pomFilter := `
		AND rp.filename LIKE '%.pom'`

	// Count query for total distinct versions
	countQuery := `
		SELECT COUNT(DISTINCT (rp.group_id, rp.artifact_id, regexp_replace(rp.version, '` + mavenReleaseVersionSuffixPattern + `', '')))
		FROM maven_mavenartifact rp
	` + innerUnion + whereClause + pomFilter

	var countTotal int
	err = conn.QueryRow(ctx, countQuery, args).Scan(&countTotal)
	if err != nil {
		return MavenVersionsResponse{}, err
	}

	args["limit"] = pageOpts.Limit
	args["offset"] = pageOpts.Offset

	query := `
		WITH version_builds AS (
			SELECT
				rp.group_id,
				rp.artifact_id,
				regexp_replace(rp.version, '` + mavenReleaseVersionSuffixPattern + `', '') as base_version,
				rp.filename,
				cc.pulp_created as created_at
			FROM maven_mavenartifact rp
			INNER JOIN core_content cc ON rp.content_ptr_id = cc.pulp_id
		` + innerUnion + whereClause + pomFilter + `
		),
		distinct_versions AS (
			SELECT
				group_id,
				artifact_id,
				base_version,
				MAX(created_at) as latest_created_at
			FROM version_builds
			GROUP BY group_id, artifact_id, base_version
			ORDER BY latest_created_at DESC
			LIMIT @limit OFFSET @offset
		)
		SELECT
			dv.group_id,
			dv.artifact_id,
			dv.base_version as version,
			COALESCE(
				JSON_AGG(
					JSON_BUILD_OBJECT(
						'version', vb.base_version,
						'filename', vb.filename,
						'created_at', vb.created_at
					) ORDER BY vb.created_at DESC
				),
				'[]'::json
			) as builds_json
		FROM distinct_versions dv
		INNER JOIN version_builds vb ON dv.group_id = vb.group_id AND dv.artifact_id = vb.artifact_id AND dv.base_version = vb.base_version
		GROUP BY dv.group_id, dv.artifact_id, dv.base_version, dv.latest_created_at
		ORDER BY dv.latest_created_at DESC`

	rows, err := conn.Query(ctx, query, args)
	if err != nil {
		return MavenVersionsResponse{}, err
	}

	type buildQueryResult struct {
		GroupID    string
		ArtifactID string
		Version    string
		BuildsJSON []byte
	}

	queryResults, err := pgx.CollectRows(rows, pgx.RowToStructByName[buildQueryResult])
	if err != nil {
		return MavenVersionsResponse{}, err
	}

	results := make([]MavenVersionsItem, 0, len(queryResults))
	for _, qr := range queryResults {
		var builds []struct {
			Version   string    `json:"version"`
			Filename  string    `json:"filename"`
			CreatedAt time.Time `json:"created_at"`
		}

		if err := json.Unmarshal(qr.BuildsJSON, &builds); err != nil {
			return MavenVersionsResponse{}, fmt.Errorf("failed to parse builds: %w", err)
		}

		buildInfos := make([]MavenBuildInfo, 0, len(builds))
		for _, b := range builds {
			buildInfos = append(buildInfos, MavenBuildInfo{
				Version:   b.Version,
				Release:   extractRelease(b.Filename),
				Filename:  b.Filename,
				CreatedAt: b.CreatedAt.Format(time.RFC3339),
			})
		}

		results = append(results, MavenVersionsItem{
			GroupID:    qr.GroupID,
			ArtifactID: qr.ArtifactID,
			Version:    qr.Version,
			Builds:     buildInfos,
		})
	}

	return MavenVersionsResponse{
		Results: results,
		Total:   countTotal,
		Limit:   pageOpts.Limit,
		Offset:  pageOpts.Offset,
	}, nil
}

// MavenRepositoryMetrics returns package, build, and version counts for the latest version of a repository.
// All counts are based on .jar artifacts. Builds are distinct full versions (e.g. 5.3.18.rhlw-00003);
// versions are distinct base versions with release qualifiers stripped (e.g. 5.3.18).
func (t *tangyImpl) MavenRepositoryMetrics(ctx context.Context, repositoryHref string) (MavenRepositoryMetrics, error) {
	if repositoryHref == "" {
		return MavenRepositoryMetrics{}, nil
	}

	conn, err := t.pool.Acquire(ctx)
	if err != nil {
		return MavenRepositoryMetrics{}, err
	}
	defer conn.Release()

	repoUUID, err := parseRepositoryHref(repositoryHref)
	if err != nil {
		return MavenRepositoryMetrics{}, fmt.Errorf("error parsing repository href: %w", err)
	}

	latestVersion, err := getLatestRepositoryVersion(ctx, conn, repoUUID)
	if err != nil {
		return MavenRepositoryMetrics{}, fmt.Errorf("error getting latest repository version: %w", err)
	}

	repoVerMap := []ParsedRepoVersion{{
		RepositoryUUID: repoUUID,
		Version:        latestVersion,
	}}

	args := pgx.NamedArgs{}
	innerUnion, err := contentIdsInVersions(ctx, conn, repoVerMap, &args)
	if err != nil {
		return MavenRepositoryMetrics{}, err
	}

	jarFilter := ` AND rp.filename LIKE '%.jar'`
	jarFrom := `
		FROM maven_mavenartifact rp
	` + innerUnion + jarFilter

	metricsQuery := `
		SELECT
			(SELECT COUNT(DISTINCT (rp.group_id, rp.artifact_id))
			` + jarFrom + `) AS package_count,
			(SELECT COUNT(DISTINCT (rp.group_id, rp.artifact_id, rp.version))
			` + jarFrom + `) AS build_count,
			(SELECT COUNT(DISTINCT (
				rp.group_id,
				rp.artifact_id,
				regexp_replace(rp.version, '` + mavenReleaseVersionSuffixPattern + `', '')
			))
			` + jarFrom + `) AS version_count`

	var metrics MavenRepositoryMetrics
	err = conn.QueryRow(ctx, metricsQuery, args).Scan(&metrics.PackageCount, &metrics.BuildCount, &metrics.VersionCount)
	if err != nil {
		return MavenRepositoryMetrics{}, err
	}

	return metrics, nil
}
