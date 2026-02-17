/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

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

package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

type resourceMeta struct {
	Identifier      types.ResourceIdentifier
	UID             string
	ResourceVersion string
	Generation      int64
}

type secretMarker struct {
	UID             string
	ResourceVersion string
	Generation      int64
}

type contentWriter struct {
	encryptor Encryptor

	mu           sync.RWMutex
	secretCache  map[string][]byte
	secretMarker map[string]secretMarker
}

func newContentWriter() *contentWriter {
	return &contentWriter{
		secretCache:  make(map[string][]byte),
		secretMarker: make(map[string]secretMarker),
	}
}

func (w *contentWriter) setEncryptor(encryptor Encryptor) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.encryptor = encryptor
}

var defaultContentWriter = newContentWriter()

// buildContentForWrite renders event content to stable ordered YAML and applies
// Secret-specific encryption when configured.
func buildContentForWrite(ctx context.Context, event Event) ([]byte, error) {
	content, err := sanitize.MarshalToOrderedYAML(event.Object)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal object to YAML: %w", err)
	}

	if !isSecretResource(event.Identifier) {
		return content, nil
	}

	return defaultContentWriter.encryptSecretContent(ctx, event, content)
}

func (w *contentWriter) encryptSecretContent(ctx context.Context, event Event, plain []byte) ([]byte, error) {
	meta := buildResourceMeta(event)
	identityKey := secretIdentityKey(meta.Identifier)
	digest := sha256.Sum256(plain)
	cacheKey := fmt.Sprintf("%s:%x", identityKey, digest)
	currentMarker := secretMarker{
		UID:             meta.UID,
		ResourceVersion: meta.ResourceVersion,
		Generation:      meta.Generation,
	}

	w.mu.RLock()
	encryptor := w.encryptor
	lastMarker, markerExists := w.secretMarker[identityKey]
	if markerExists && lastMarker == currentMarker {
		if cached, ok := w.secretCache[cacheKey]; ok {
			if metrics.SecretEncryptionMarkerSkipsTotal != nil {
				metrics.SecretEncryptionMarkerSkipsTotal.Add(ctx, 1)
			}
			if metrics.SecretEncryptionCacheHitsTotal != nil {
				metrics.SecretEncryptionCacheHitsTotal.Add(ctx, 1)
			}
			w.mu.RUnlock()
			return append([]byte(nil), cached...), nil
		}
	}
	w.mu.RUnlock()

	if encryptor == nil {
		return nil, errors.New("secret encryption is required but no encryptor is configured")
	}

	if metrics.SecretEncryptionAttemptsTotal != nil {
		metrics.SecretEncryptionAttemptsTotal.Add(ctx, 1)
	}
	encrypted, err := encryptor.Encrypt(ctx, plain, ResourceMeta{
		Identifier:      meta.Identifier,
		UID:             meta.UID,
		ResourceVersion: meta.ResourceVersion,
		Generation:      meta.Generation,
	})
	if err != nil {
		if metrics.SecretEncryptionFailuresTotal != nil {
			metrics.SecretEncryptionFailuresTotal.Add(ctx, 1)
		}
		return nil, fmt.Errorf("secret encryption failed: %w", err)
	}
	if metrics.SecretEncryptionSuccessTotal != nil {
		metrics.SecretEncryptionSuccessTotal.Add(ctx, 1)
	}

	w.mu.Lock()
	w.secretCache[cacheKey] = append([]byte(nil), encrypted...)
	w.secretMarker[identityKey] = currentMarker
	w.mu.Unlock()

	return encrypted, nil
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

func secretIdentityKey(id types.ResourceIdentifier) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", id.Group, id.Version, id.Resource, id.Namespace, id.Name)
}

func isSecretResource(id types.ResourceIdentifier) bool {
	return id.Group == "" && id.Version == "v1" && id.Resource == "secrets"
}
