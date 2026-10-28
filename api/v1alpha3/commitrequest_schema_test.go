// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// commitRequestSchemaHashes is the hash of the generated CommitRequest CRD at each schema revision,
// with the revision line itself left out. A change to the CRD that keeps the revision fails
// TestCommitRequestSchemaRevision: raise CommitRequestSchemaRevision, and the revision line, and add
// the new hash here.
var commitRequestSchemaHashes = map[int]string{
	1: "395df39c86bc4c0350eb2c476dd0ae02db2a7513f03962103416123b0cbb5476",
	2: "015bf5e198330632ded5c43fd02e5f4974f94c5f01acfbacd205ac484377099c",
}

// commitRequestRevisionLine is the last line of the kind's description.
var commitRequestRevisionLine = regexp.MustCompile(
	`Schema revision (\d+), compatible from revision (\d+)\.`)

// The CommitRequest CRD carries its schema revision as the last line of the kind's description, and
// the revision moves with every change to the generated CRD, so a cluster that installed it on its own
// can be judged by it.
func TestCommitRequestSchemaRevision(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/configbutler.ai_commitrequests.yaml")
	require.NoError(t, err)
	crd := string(raw)

	match := commitRequestRevisionLine.FindStringSubmatch(crd)
	require.NotNil(t, match, "the CRD's description must end with the schema revision line")
	revision, _ := strconv.Atoi(match[1])
	floor, _ := strconv.Atoi(match[2])
	require.Equal(t, CommitRequestSchemaRevision, revision, "the revision line and CommitRequestSchemaRevision")
	require.Equal(t, CommitRequestSchemaFloor, floor, "the revision line and CommitRequestSchemaFloor")
	require.LessOrEqual(t, floor, revision)

	sum := sha256.Sum256([]byte(strings.Replace(crd, match[0], "", 1)))
	hash := hex.EncodeToString(sum[:])
	want, recorded := commitRequestSchemaHashes[revision]
	require.True(t, recorded, "revision %d has no hash recorded in commitRequestSchemaHashes", revision)
	require.Equal(t, want, hash, "the generated CommitRequest CRD changed at revision %d: raise the schema "+
		"revision in api/v1alpha3/commitrequest_types.go, and record the new CRD's hash for it in "+
		"commitRequestSchemaHashes", revision)
}
