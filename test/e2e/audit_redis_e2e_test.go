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

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
)

const (
	defaultAuditRedisStream = "gitopsreverser.audit.events.v1"
)

var _ = Describe("Audit Redis Queue", Label("audit-redis"), Ordered, func() {
	It("should enqueue incoming audit webhook events into a Redis stream", func() {
		streamName := defaultAuditRedisStream
		testConfigMapName := fmt.Sprintf("audit-redis-test-cm-%d", GinkgoRandomSeed())

		By("connecting to Valkey through the e2e port-forward")
		client := redis.NewClient(&redis.Options{
			Addr: valkeyPortForwardAddr(),
		})
		Eventually(func(g Gomega) {
			g.Expect(client.Ping(context.Background()).Err()).NotTo(HaveOccurred())
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("clearing the dedicated stream for deterministic assertions")
		_, err := client.Del(context.Background(), streamName).Result()
		Expect(err).NotTo(HaveOccurred())

		By("triggering kube-apiserver audit events with a ConfigMap create/update")
		_, err = kubectlRunInNamespace(
			namespace,
			"delete",
			"configmap",
			testConfigMapName,
			"--ignore-not-found=true",
		)
		Expect(err).NotTo(HaveOccurred())

		_, err = kubectlRunInNamespace(
			namespace,
			"create",
			"configmap",
			testConfigMapName,
			"--from-literal=test=audit-redis",
		)
		Expect(err).NotTo(HaveOccurred())

		_, err = kubectlRunInNamespace(
			namespace,
			"patch",
			"configmap",
			testConfigMapName,
			"--type=merge",
			"--patch",
			`{"data":{"test":"audit-redis-updated"}}`,
		)
		Expect(err).NotTo(HaveOccurred())

		By("verifying Redis stream receives an entry for the ConfigMap audit event")
		Eventually(func(g Gomega) {
			entries, readErr := client.XRange(context.Background(), streamName, "-", "+").Result()
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(entries).NotTo(BeEmpty())

			found := false
			for _, entry := range entries {
				resource, _ := entry.Values["resource"].(string)
				name, _ := entry.Values["name"].(string)
				clusterID, _ := entry.Values["cluster_id"].(string)
				payload, _ := entry.Values["payload_json"].(string)
				if resource == "configmaps" && name == testConfigMapName && clusterID == "kind-e2e" && payload != "" {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "expected ConfigMap audit event to be enqueued in Redis stream")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("cleaning up test ConfigMap")
		_, _ = kubectlRunInNamespace(
			namespace,
			"delete",
			"configmap",
			testConfigMapName,
			"--ignore-not-found=true",
		)
	})
})

func valkeyPortForwardAddr() string {
	port := strings.TrimSpace(os.Getenv("VALKEY_PORT"))
	if port == "" {
		port = "16379"
	}
	return "127.0.0.1:" + port
}
