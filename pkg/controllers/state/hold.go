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

package state

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	"sigs.k8s.io/karpenter/pkg/operator/options"
)

// loft-sh: a node that an external system is repairing in place (cordon, drain, reboot) is "held". A held node
// is expected back shortly, so it must not be disrupted, and it still counts as capacity for pending pods even
// though it is cordoned and may be NotReady.
//
// Both settings come from the options: NodeHoldLabelKey is the label that the external system keeps on the node
// while it repairs it, and NodeHoldDuration bounds the hold. Nodes are never held if the label key is empty.

// nodeClock lets a StateNode tell the time by itself, so that Held() needs no clock from its callers.
// It is a struct with its own DeepCopyInto because the deepcopy generator can't handle an interface field.
type nodeClock struct {
	clock.Clock
}

func (in *nodeClock) DeepCopyInto(out *nodeClock) {
	*out = *in
}

// isNodeUnderRepair returns true if the node carries the hold label. The value of the label is deliberately not
// checked: NVSentinel, for one, reports "remediation-failed" after a successful reboot until the GPU driver is back,
// so a failure value does not mean that the node is lost.
func isNodeUnderRepair(node *corev1.Node, labelKey string) bool {
	if node == nil || labelKey == "" {
		return false
	}
	_, ok := node.Labels[labelKey]
	return ok
}

// heldUntil returns when the hold of the node ends, or the zero time if the node isn't under repair. The hold is
// counted from when the repair was first observed, so that a node that never comes back gets the stock behavior
// again: it is replaced and does not keep pods pending.
// The time is only kept in memory, so the hold starts over when Karpenter restarts.
func heldUntil(ctx context.Context, node *corev1.Node, oldNode *StateNode, now time.Time) metav1.Time {
	opts := options.FromContext(ctx)
	if !isNodeUnderRepair(node, opts.NodeHoldLabelKey) {
		return metav1.Time{}
	}
	if oldNode != nil && !oldNode.heldUntil.IsZero() {
		return oldNode.heldUntil
	}
	return metav1.Time{Time: now.Add(opts.NodeHoldDuration)}
}

// Held returns true if the node is being repaired in place and is still expected back.
// Only registered nodes with a NodeClaim can be held, since the NodeClaim is what describes the node while its own
// taints and resources are unreliable.
func (in *StateNode) Held() bool {
	if in.Node == nil || in.NodeClaim == nil || in.clock.Clock == nil || in.heldUntil.IsZero() {
		return false
	}
	if !in.Registered() {
		return false
	}
	return in.clock.Now().Before(in.heldUntil.Time)
}
