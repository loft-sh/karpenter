/*
Copyright The Kubernetes Authors.

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

package scheduling_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// loft-sh: tests for nodes that are held while an external system (NVSentinel, for one) repairs them in place
var _ = Describe("Held Nodes", func() {
	const holdLabelKey = "example.com/under-repair"
	const holdDuration = 20 * time.Minute
	var nodePool *v1.NodePool
	var gpuPod test.PodOptions
	var node *corev1.Node

	BeforeEach(func() {
		fakeClock.SetTime(time.Now())
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
			NodeHoldLabelKey: lo.ToPtr(holdLabelKey),
			NodeHoldDuration: lo.ToPtr(holdDuration),
		}))
		nodePool = test.NodePool()
		gpuPod = test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{fake.ResourceGPUVendorA: resource.MustParse("1")},
		}}
		ExpectApplied(ctx, env.Client, nodePool)

		// a GPU node that ran a pod, which then got evicted by a drain
		pod := test.UnschedulablePod(gpuPod)
		bindings := ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node = ExpectScheduled(ctx, env.Client, pod)
		nodeClaim := bindings.Get(pod).NodeClaim
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectDeleted(ctx, env.Client, pod)
	})
	AfterEach(func() {
		ctx = options.ToContext(ctx, test.Options())
		fakeClock.SetTime(time.Now())
	})

	// cordonAndReboot makes the node look like it does while it is rebooting: cordoned, not ready and without GPUs
	cordonAndReboot := func(labels map[string]string) {
		GinkgoHelper()
		node = ExpectExists(ctx, env.Client, node)
		for k, v := range labels {
			node.Labels[k] = v
		}
		node.Spec.Unschedulable = true
		node.Spec.Taints = append(node.Spec.Taints,
			corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule},
			corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule},
			corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute},
		)
		node.Status.Capacity[fake.ResourceGPUVendorA] = resource.MustParse("0")
		node.Status.Allocatable[fake.ResourceGPUVendorA] = resource.MustParse("0")
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
	}

	It("should count a held node as capacity for pending pods", func() {
		cordonAndReboot(map[string]string{holdLabelKey: "remediating"})

		pod := test.UnschedulablePod(gpuPod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).To(Equal(node.Name))
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
	})
	It("should launch new capacity when a cordoned node isn't held", func() {
		cordonAndReboot(nil)

		pod := test.UnschedulablePod(gpuPod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
	})
	It("should launch new capacity once the hold has expired", func() {
		cordonAndReboot(map[string]string{holdLabelKey: "remediation-failed"})
		fakeClock.Step(holdDuration)

		pod := test.UnschedulablePod(gpuPod)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).ToNot(Equal(node.Name))
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
	})
})
