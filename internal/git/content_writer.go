// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

type resourceMeta struct {
	Identifier      types.ResourceIdentifier
	UID             string
	ResourceVersion string
	Generation      int64
}

type sensitiveMarker struct {
	UID             string
	ResourceVersion string
	Generation      int64
}

type contentWriter struct {
	encryptor          Encryptor
	sensitiveResources types.SensitiveResourcePolicy
	// encryptionScope partitions encryption cache entries so cached bytes never cross
	// repo/path/key boundaries (e.g. different GitTargets or rotated identities).
	encryptionScope string

	mu             sync.RWMutex
	encryptedCache map[string][]byte
	markers        map[string]sensitiveMarker
}

type eventContentWriter interface {
	buildContentForWrite(ctx context.Context, event Event) ([]byte, error)
	filePathForIdentifier(id types.ResourceIdentifier) string
	isSensitiveIdentifier(id types.ResourceIdentifier) bool
}

func newContentWriter(sensitiveResources types.SensitiveResourcePolicy) *contentWriter {
	return &contentWriter{
		sensitiveResources: sensitiveResources,
		encryptedCache:     make(map[string][]byte),
		markers:            make(map[string]sensitiveMarker),
	}
}

func (w *contentWriter) setEncryptor(encryptor Encryptor, scope string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.encryptor = encryptor
	w.encryptionScope = scope
}

// buildContentForWrite renders event content to stable ordered YAML and applies
// sensitive-resource encryption when configured.
func (w *contentWriter) buildContentForWrite(ctx context.Context, event Event) ([]byte, error) {
	content, err := sanitize.MarshalToOrderedYAML(event.Object)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal object to YAML: %w", err)
	}

	if !w.isSensitiveIdentifier(event.Identifier) {
		return content, nil
	}

	return w.encryptSensitiveContent(ctx, event, content)
}

func (w *contentWriter) filePathForIdentifier(id types.ResourceIdentifier) string {
	return generateFilePath(id, w.sensitiveResources)
}

func (w *contentWriter) isSensitiveIdentifier(id types.ResourceIdentifier) bool {
	return w.sensitiveResources.IsSensitive(id.Group, id.Resource)
}

func (w *contentWriter) encryptSensitiveContent(ctx context.Context, event Event, plain []byte) ([]byte, error) {
	meta := buildResourceMeta(event)
	identityKey := meta.Identifier.Key()
	digest := sha256.Sum256(plain)
	currentMarker := sensitiveMarker{
		UID:             meta.UID,
		ResourceVersion: meta.ResourceVersion,
		Generation:      meta.Generation,
	}

	w.mu.RLock()
	encryptor := w.encryptor
	scope := w.encryptionScope
	w.mu.RUnlock()

	scopedIdentityKey := identityKey
	if strings.TrimSpace(scope) != "" {
		scopedIdentityKey = fmt.Sprintf("%s:%s", scope, identityKey)
	}

	cacheKey := fmt.Sprintf("%s:%x", scopedIdentityKey, digest)

	w.mu.RLock()
	cached, ok := w.cachedEncryptedContent(ctx, scopedIdentityKey, cacheKey, currentMarker)
	w.mu.RUnlock()
	if ok {
		return cached, nil
	}

	if encryptor == nil {
		return nil, errors.New("secret encryption is required but no encryptor is configured")
	}

	encrypted, err := encryptor.Encrypt(ctx, plain, ResourceMeta(meta))
	if err != nil {
		recordSecretEncryption(ctx, secretEncryptionFailed)
		return nil, fmt.Errorf("secret encryption failed: %w", err)
	}
	recordSecretEncryption(ctx, secretEncryptionEncrypted)

	w.mu.Lock()
	w.encryptedCache[cacheKey] = append([]byte(nil), encrypted...)
	w.markers[scopedIdentityKey] = currentMarker
	w.mu.Unlock()

	return encrypted, nil
}

func (w *contentWriter) cachedEncryptedContent(
	ctx context.Context,
	identityKey, cacheKey string,
	currentMarker sensitiveMarker,
) ([]byte, bool) {
	lastMarker, markerExists := w.markers[identityKey]
	if !markerExists || lastMarker != currentMarker {
		return nil, false
	}
	cached, ok := w.encryptedCache[cacheKey]
	if !ok {
		return nil, false
	}
	recordSecretEncryption(ctx, secretEncryptionCached)
	return append([]byte(nil), cached...), true
}

func buildResourceMeta(event Event) resourceMeta {
	meta := resourceMeta{
		Identifier: event.Identifier,
	}
	if event.Object == nil {
		return meta
	}
	meta.UID = string(event.Object.GetUID())
	meta.ResourceVersion = event.Object.GetResourceVersion()
	meta.Generation = event.Object.GetGeneration()
	return meta
}

// Secret encryption outcome label values. The set partitions every decision the encryption path
// makes about one document: it ran and produced ciphertext, it ran and failed, or it did not run
// because unchanged content was reused.
const (
	secretEncryptionEncrypted = "encrypted"
	secretEncryptionFailed    = "failed"
	secretEncryptionCached    = "cached"
)

// recordSecretEncryption counts one encryption decision.
//
// One counter with an outcome label, where there used to be five counters over two populations:
// `attempts` was incremented immediately before Encrypt and so was exactly success + failures, and
// `cache_hits` and `marker_skips` were incremented on consecutive lines of one branch and could
// never differ. A share of one total is also the ratio the old "cache effectiveness" query was
// reaching for, which divided two counters over disjoint populations.
func recordSecretEncryption(ctx context.Context, outcome string) {
	if telemetry.SecretEncryptionsTotal == nil {
		return
	}
	telemetry.SecretEncryptionsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
