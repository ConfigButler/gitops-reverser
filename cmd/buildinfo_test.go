// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setBuildVars overrides the ldflags-injected build vars for a test and returns
// a function that restores their original values.
func setBuildVars(v, commit, dirty, date string) func() {
	origV, origC, origD, origDate := version, gitCommit, gitDirty, buildDate
	version, gitCommit, gitDirty, buildDate = v, commit, dirty, date
	return func() {
		version, gitCommit, gitDirty, buildDate = origV, origC, origD, origDate
	}
}

func TestCurrentBuildInfo_Clean(t *testing.T) {
	defer setBuildVars("1.2.3", "abc123", "0", "2026-05-22T00:00:00Z")()

	bi := currentBuildInfo()
	assert.Equal(t, "1.2.3", bi.Version)
	assert.Equal(t, "abc123", bi.GitCommit)
	assert.False(t, bi.IsDirty)
	assert.Equal(t, "abc123", bi.CommitWithDirty)
	assert.Equal(t, "2026-05-22T00:00:00Z", bi.BuildDate)
	assert.Equal(t, runtime.Version(), bi.GoVersion)
}

func TestCurrentBuildInfo_Dirty(t *testing.T) {
	defer setBuildVars("dev", "abc123", "1", "unknown")()

	bi := currentBuildInfo()
	assert.True(t, bi.IsDirty)
	assert.Equal(t, "abc123-dirty", bi.CommitWithDirty)
}

func TestBuildInfoHandler_Get(t *testing.T) {
	defer setBuildVars("9.9.9", "deadbeef", "0", "2026-01-01T00:00:00Z")()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/build-info", nil)
	buildInfoHandler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

	var got buildInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "9.9.9", got.Version)
	assert.Equal(t, "deadbeef", got.CommitWithDirty)
	assert.False(t, got.IsDirty)
}

func TestBuildInfoHandler_RejectsNonGet(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/build-info", nil)
	buildInfoHandler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
