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

package state_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// loft-sh: tests for nodes that are held while an external system (NVSentinel, for one) repairs them in place
var _ = Describe("Held Nodes", func() {
	const gpu = corev1.ResourceName("example.com/gpu")
	const holdLabelKey = "example.com/under-repair"
	const holdDuration = 20 * time.Minute
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	var nodeClaimTaint, cordonTaint, notReadyTaint corev1.Taint

	BeforeEach(func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
			NodeHoldLabelKey: lo.ToPtr(holdLabelKey),
			NodeHoldDuration: lo.ToPtr(holdDuration),
		}))
		nodeClaimTaint = corev1.Taint{Key: "example.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule}
		cordonTaint = corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}
		notReadyTaint = corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute}
		resources := corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("4"),
			gpu:                resource.MustParse("2"),
		}
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
				},
			},
			Spec: v1.NodeClaimSpec{
				Taints: []corev1.Taint{nodeClaimTaint},
			},
			Status: v1.NodeClaimStatus{
				Capacity:    resources,
				Allocatable: resources,
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeController, nodeClaimController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
	})

	AfterEach(func() {
		ctx = options.ToContext(ctx, test.Options())
	})

	// startRepair makes the node look like NVSentinel cordoned it and a reboot took the GPUs away
	startRepair := func() {
		GinkgoHelper()
		node = ExpectExists(ctx, env.Client, node)
		node.Labels[holdLabelKey] = "quarantined"
		node.Spec.Unschedulable = true
		node.Spec.Taints = []corev1.Taint{nodeClaimTaint, cordonTaint, notReadyTaint}
		node.Status.Capacity[gpu] = resource.MustParse("0")
		node.Status.Allocatable[gpu] = resource.MustParse("0")
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
	}
	finishRepair := func() {
		GinkgoHelper()
		node = ExpectExists(ctx, env.Client, node)
		delete(node.Labels, holdLabelKey)
		node.Spec.Unschedulable = false
		node.Spec.Taints = []corev1.Taint{nodeClaimTaint}
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
	}
	updateNode := func() {
		GinkgoHelper()
		node = ExpectExists(ctx, env.Client, node)
		node.Labels[holdLabelKey] = "remediation-failed"
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
	}

	It("should not hold a node that isn't under repair", func() {
		stateNode := ExpectStateNodeExists(cluster, node)
		Expect(stateNode.Held()).To(BeFalse())
		Expect(stateNode.ValidateNodeDisruptable(fakeClock)).To(Succeed())
	})
	It("should not hold a node under repair when no hold label key is configured", func() {
		ctx = options.ToContext(ctx, test.Options())
		startRepair()

		stateNode := ExpectStateNodeExists(cluster, node)
		Expect(stateNode.Held()).To(BeFalse())
		Expect(stateNode.Taints()).To(ContainElement(cordonTaint))
		Expect(stateNode.Allocatable()[gpu]).To(Equal(resource.MustParse("0")))
		Expect(stateNode.ValidateNodeDisruptable(fakeClock)).To(Succeed())
	})
	It("should not hold a cordoned node without the repair label", func() {
		node = ExpectExists(ctx, env.Client, node)
		node.Spec.Unschedulable = true
		node.Spec.Taints = []corev1.Taint{nodeClaimTaint, cordonTaint}
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		stateNode := ExpectStateNodeExists(cluster, node)
		Expect(stateNode.Held()).To(BeFalse())
		Expect(stateNode.Taints()).To(ContainElement(cordonTaint))
	})
	It("should hold a node under repair and describe it by its NodeClaim", func() {
		startRepair()
		stateNode := ExpectStateNodeExists(cluster, node)
		Expect(stateNode.Held()).To(BeTrue())

		// the cordon and not-ready taints are ignored
		Expect(stateNode.Taints()).To(ConsistOf(nodeClaimTaint))
		// resources that the node reports as zero are taken from the NodeClaim
		Expect(stateNode.Allocatable()[gpu]).To(Equal(resource.MustParse("2")))
		Expect(stateNode.Capacity()[gpu]).To(Equal(resource.MustParse("2")))
		Expect(stateNode.Available()[gpu]).To(Equal(resource.MustParse("2")))
		// and the node can't be disrupted
		Expect(stateNode.ValidateNodeDisruptable(fakeClock)).To(MatchError(ContainSubstring("node is held")))
	})
	It("should keep holding a deep copy of the node", func() {
		startRepair()
		nodes := cluster.DeepCopyNodes()
		Expect(nodes).To(HaveLen(1))
		Expect(nodes[0].Held()).To(BeTrue())
	})
	It("should count the hold from when the repair was first seen, across node and nodeclaim updates", func() {
		startRepair()
		fakeClock.Step(15 * time.Minute)
		updateNode()
		Expect(ExpectStateNodeExists(cluster, node).Held()).To(BeTrue())

		fakeClock.Step(holdDuration - 15*time.Minute - time.Second)
		Expect(ExpectStateNodeExists(cluster, node).Held()).To(BeTrue())
		fakeClock.Step(time.Second)
		Expect(ExpectStateNodeExists(cluster, node).Held()).To(BeFalse())
	})
	It("should give a node the stock behavior again once the hold has expired", func() {
		startRepair()
		fakeClock.Step(holdDuration)
		updateNode()

		stateNode := ExpectStateNodeExists(cluster, node)
		Expect(stateNode.Held()).To(BeFalse())
		Expect(stateNode.Taints()).To(ConsistOf(nodeClaimTaint, cordonTaint, notReadyTaint))
		Expect(stateNode.Allocatable()[gpu]).To(Equal(resource.MustParse("0")))
		Expect(stateNode.ValidateNodeDisruptable(fakeClock)).To(Succeed())
	})
	It("should stop holding a node when the repair is over, and hold it again for a new repair", func() {
		startRepair()
		fakeClock.Step(holdDuration - time.Minute)
		finishRepair()
		Expect(ExpectStateNodeExists(cluster, node).Held()).To(BeFalse())

		// a new repair gets the full duration again
		startRepair()
		fakeClock.Step(holdDuration - time.Minute)
		Expect(ExpectStateNodeExists(cluster, node).Held()).To(BeTrue())
	})
})
