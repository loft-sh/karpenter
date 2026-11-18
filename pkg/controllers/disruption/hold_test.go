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

package disruption_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// loft-sh: tests for nodes that are held while an external system (NVSentinel, for one) repairs them in place
var _ = Describe("Held Nodes", func() {
	const holdLabelKey = "example.com/under-repair"
	const holdDuration = 20 * time.Minute
	var nodePool *v1.NodePool
	var nodeClaims []*v1.NodeClaim
	var nodes []*corev1.Node

	BeforeEach(func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
			NodeHoldLabelKey: lo.ToPtr(holdLabelKey),
			NodeHoldDuration: lo.ToPtr(holdDuration),
		}))
		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
					// Disrupt away!
					Budgets: []v1.Budget{{
						Nodes: "100%",
					}},
				},
			},
		})
		nodeClaims, nodes = test.NodeClaimsAndNodes(2, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
					v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:  resource.MustParse("32"),
					corev1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		for _, nc := range nodeClaims {
			nc.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
		}
	})

	AfterEach(func() {
		ctx = options.ToContext(ctx, test.Options())
	})

	// hold makes the node look like NVSentinel cordoned it for a repair
	hold := func(node *corev1.Node) {
		node.Labels = lo.Assign(node.Labels, map[string]string{holdLabelKey: "drain-succeeded"})
		node.Spec.Unschedulable = true
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule})
	}

	It("should not delete an empty node that is held", func() {
		hold(nodes[0])
		ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodes[0])
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})

		ExpectSingletonReconciled(ctx, disruptionController)

		Expect(queue.GetCommands()).To(BeEmpty())
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
		ExpectExists(ctx, env.Client, nodeClaims[0])
	})
	It("should delete an empty node once the hold has expired", func() {
		hold(nodes[0])
		ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodes[0])
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})
		fakeClock.Step(holdDuration)

		ExpectSingletonReconciled(ctx, disruptionController)
		ExpectObjectReconciled(ctx, env.Client, queue, nodeClaims[0])
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaims[0])

		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, nodeClaims[0], nodes[0])
	})
	It("should not delete a node because its pods would fit on a held node", func() {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
		})
		// the first node is drained and held, the second one runs a pod that would fit on the first one
		hold(nodes[0])
		ExpectApplied(ctx, env.Client, rs, pod, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)
		ExpectManualBinding(ctx, env.Client, pod, nodes[1])
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1]})

		ExpectSingletonReconciled(ctx, disruptionController)

		Expect(queue.GetCommands()).To(BeEmpty())
		Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
		Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
	})
})
