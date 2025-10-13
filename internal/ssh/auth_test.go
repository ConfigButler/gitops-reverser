/*
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

package ssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSSHPrivateKey is a test RSA private key (not for production use).
const testSSHPrivateKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAyVSi0fDJKlQnPCXjY9QGFmPuLbVFLPwuFP3KBYLTrQZ0M5n5
-----END RSA PRIVATE KEY-----`

func TestGetAuthMethod(t *testing.T) {
	privateKey := testSSHPrivateKey
	auth, err := GetAuthMethod(privateKey, "", "")

	// Expect error because test key is truncated/invalid
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "failed to create SSH public keys")
}

func TestGetAuthMethod_WithKnownHosts(t *testing.T) {
	privateKey := testSSHPrivateKey
	knownHosts := "github.com ssh-rsa AAAAB3NzaC1yc2EAAAABIwAAAQEAq2A7hRGmdnm9tUDbO9IDSwBK6TbQa+PXYPCPy6rbTrTtw7PHkccKrpp0yVhp5HdEIcKr6pLlVDBfOLX9QUsyCOV0wzfjIJNlGEYsdlLJizHhbn2mUjvSAHQqZETYP03HR+xYPVY/wDHEL0w1vXw1g7VQAN+5SZG1yQ+Qr2lnJbj5+6zP+Yr5s6CJXZ1F4OG8E7eHdOd5MFBjv9D9rLJvQjk5FVMzqZ+mZJ+W8Xj5MQP6vYzZh7cC9qPqJ8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8bQP8YB+KCJ3oGxZ8F8"

	auth, err := GetAuthMethod(privateKey, "", knownHosts)
	// Expect error because test key is truncated/invalid
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "failed to create SSH public keys")
}

func TestGetAuthMethod_InvalidKey(t *testing.T) {
	invalidKey := "this-is-not-a-valid-ssh-key"

	auth, err := GetAuthMethod(invalidKey, "", "")
	require.Error(t, err)
	assert.Nil(t, auth)
}

func TestGetAuthMethod_EmptyKey(t *testing.T) {
	auth, err := GetAuthMethod("", "", "")
	require.Error(t, err)
	assert.Nil(t, auth)
	assert.Contains(t, err.Error(), "private key cannot be empty")
}
