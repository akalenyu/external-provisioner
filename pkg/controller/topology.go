/*
Copyright 2018 The Kubernetes Authors.

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

package controller

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/rpc"
	"github.com/kubernetes-csi/external-provisioner/pkg/features"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
)

// topologyTerm represents a single term where its topology key value pairs are AND'd together.
type topologyTerm map[string]string

func GenerateVolumeNodeAffinity(accessibleTopology []*csi.Topology) *v1.VolumeNodeAffinity {
	if len(accessibleTopology) == 0 {
		return nil
	}

	var terms []v1.NodeSelectorTerm
	for _, topology := range accessibleTopology {
		if len(topology.Segments) == 0 {
			continue
		}

		var expressions []v1.NodeSelectorRequirement
		for k, v := range topology.Segments {
			expressions = append(expressions, v1.NodeSelectorRequirement{
				Key:      k,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v},
			})
		}
		terms = append(terms, v1.NodeSelectorTerm{
			MatchExpressions: expressions,
		})
	}

	return &v1.VolumeNodeAffinity{
		Required: &v1.NodeSelector{
			NodeSelectorTerms: terms,
		},
	}
}

// VolumeIsAccessible checks whether the generated volume affinity is satisfied by
// a the node topology that a CSI driver reported in GetNodeInfoResponse.
func VolumeIsAccessible(affinity *v1.VolumeNodeAffinity, nodeTopology *csi.Topology) (bool, error) {
	if nodeTopology == nil || affinity == nil || affinity.Required == nil {
		// No topology information -> all volumes accessible.
		return true, nil
	}

	nodeLabels := labels.Set{}
	for k, v := range nodeTopology.Segments {
		nodeLabels[k] = v
	}
	node := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: nodeLabels,
		},
	}
	return corev1helpers.MatchNodeSelectorTerms(&node, affinity.Required)
}

// SupportsTopology returns whether topology is supported both for plugin and external provisioner
func SupportsTopology(pluginCapabilities rpc.PluginCapabilitySet) bool {
	return pluginCapabilities[csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS] &&
		utilfeature.DefaultFeatureGate.Enabled(features.Topology)
}

// GenerateAccessibilityRequirements returns the CSI TopologyRequirement
// to pass into the CSI CreateVolume request.
//
// This function is called if the topology feature is enabled
// in the external-provisioner and the CSI driver implements the
// CSI accessibility capability. It is disabled by default.
//
// If enabled, we require that the K8s API server is on at least
// K8s 1.17 and that the K8s Nodes are on at least K8s 1.15 in
// accordance with the 2 version skew between control plane and
// nodes.
//
// There are two main cases to consider:
//
// 1) selectedNode is not set (immediate binding):
//
//    In this case, we list all CSINode objects to find a Node that
//    the driver has registered topology keys with.
//
//    Once we get the list of CSINode objects, we find one that has
//    topology keys registered. If none are found, then we assume
//    that the driver has not started on any node yet, and we error
//    and retry.
//
//    If at least one CSINode object is found with topology keys,
//    then we continue and use that for assembling the topology
//    requirement. The available topologies will be limited to the
//    Nodes that the driver has registered with.
//
// 2) selectedNode is set (delayed binding):
//
//    We will get the topology from the CSINode object for the selectedNode
//    and error if we can't (and retry).
//
func GenerateAccessibilityRequirements(
	kubeClient kubernetes.Interface,
	driverName string,
	pvcName string,
	allowedTopologies []v1.TopologySelectorTerm,
	selectedNode *v1.Node,
	strictTopology bool,
	immediateTopology bool,
	csiNodeLister storagelistersv1.CSINodeLister,
	nodeLister corelisters.NodeLister) (*csi.TopologyRequirement, error) {
	requirement := &csi.TopologyRequirement{}

	var (
		selectedCSINode  *storagev1.CSINode
		selectedTopology topologyTerm
		requisiteTerms   []topologyTerm
		err              error
	)

	// 1. Get CSINode for the selected node
	if selectedNode != nil {
		selectedCSINode, err = getSelectedCSINode(csiNodeLister, selectedNode)
		if err != nil {
			return nil, err
		}
		topologyKeys := getTopologyKeys(selectedCSINode, driverName)
		if len(topologyKeys) == 0 {
			// The scheduler selected a node with no topology information.
			// This can happen if:
			//
			// * the node driver is not deployed on all nodes.
			// * the node driver is being restarted and has not re-registered yet. This should be
			//   temporary and a retry should eventually succeed.
			//
			// Returning an error in provisioning will cause the scheduler to retry and potentially
			// (but not guaranteed) pick a different node.
			return nil, fmt.Errorf("no topology key found on CSINode %s", selectedCSINode.Name)
		}
		var isMissingKey bool
		selectedTopology, isMissingKey = getTopologyFromNode(selectedNode, topologyKeys)
		if isMissingKey {
			return nil, fmt.Errorf("topology labels from selected node %v does not match topology keys from CSINode %v", selectedNode.Labels, topologyKeys)
		}

		if strictTopology {
			// Make sure that selected node topology is in allowed topologies list
			if len(allowedTopologies) != 0 {
				allowedTopologiesFlatten := flatten(allowedTopologies)
				found := false
				for _, t := range allowedTopologiesFlatten {
					if t.subset(selectedTopology) {
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("selected node '%q' topology '%v' is not in allowed topologies: %v", selectedNode.Name, selectedTopology, allowedTopologiesFlatten)
				}
			}
			// Only pass topology of selected node.
			requisiteTerms = append(requisiteTerms, selectedTopology)
		}
	}

	// 2. Generate CSI Requisite Terms
	if len(requisiteTerms) == 0 {
		if len(allowedTopologies) != 0 {
			// Distribute out one of the OR layers in allowedTopologies
			requisiteTerms = flatten(allowedTopologies)
		} else {
			if selectedNode == nil && !immediateTopology {
				// Don't specify any topology requirements because neither the PVC nor
				// the storage class have limitations and the CSI driver is not interested
				// in being told where it runs (perhaps it already knows, for example).
				return nil, nil
			}

			// Aggregate existing topologies in nodes across the entire cluster.
			requisiteTerms, err = aggregateTopologies(driverName, selectedCSINode, csiNodeLister, nodeLister)
			if err != nil {
				return nil, err
			}
			if len(requisiteTerms) == 0 {
				// We may reach here if the driver has not registered on any nodes.
				// We should wait for at least one driver to start so that we can
				// provision in a supported topology.
				return nil, fmt.Errorf("no available topology found")
			}
		}
	}

	// It might be possible to reach here if allowedTopologies had empty entries.
	// We fallback to the "topology disabled" behavior.
	if len(requisiteTerms) == 0 {
		return nil, nil
	}

	requisiteTerms = deduplicate(requisiteTerms)
	// TODO (verult) reduce subset duplicate terms (advanced reduction)

	requirement.Requisite = toCSITopology(requisiteTerms)

	// 3. Generate CSI Preferred Terms
	var preferredTerms []topologyTerm
	if selectedCSINode == nil {
		// Immediate binding, we fallback to statefulset spreading hash for backwards compatibility.

		// Ensure even spreading of StatefulSet volumes by sorting
		// requisiteTerms and shifting the sorted terms based on hash of pvcName and replica index suffix
		hash, index := getPVCNameHashAndIndexOffset(pvcName)
		i := (hash + index) % uint32(len(requisiteTerms))
		preferredTerms = sortAndShift(requisiteTerms, nil, i)
	} else {
		// Delayed binding, use topology from that node to populate preferredTerms
		if strictTopology {
			// In case of strict topology, preferred = requisite
			preferredTerms = requisiteTerms
		} else {
			preferredTerms = sortAndShift(requisiteTerms, selectedTopology, 0)
			if preferredTerms == nil {
				// Topology from selected node is not in requisite. This case should never be hit:
				// - If AllowedTopologies is specified, the scheduler should choose a node satisfying the
				//   constraint.
				// - Otherwise, the aggregated topology is guaranteed to contain topology information from the
				//   selected node.
				return nil, fmt.Errorf("topology %v from selected node %q is not in requisite: %v", selectedTopology, selectedNode.Name, requisiteTerms)
			}
		}
	}
	requirement.Preferred = toCSITopology(preferredTerms)
	return requirement, nil
}

// getSelectedCSINode returns the CSINode object for the given selectedNode.
func getSelectedCSINode(
	csiNodeLister storagelistersv1.CSINodeLister,
	selectedNode *v1.Node) (*storagev1.CSINode, error) {

	selectedCSINode, err := csiNodeLister.Get(selectedNode.Name)
	if err != nil {
		// We don't want to fallback and provision in the wrong topology if there's some temporary
		// error with the API server.
		return nil, fmt.Errorf("error getting CSINode for selected node %q: %v", selectedNode.Name, err)
	}
	if selectedCSINode == nil {
		return nil, fmt.Errorf("CSINode for selected node %q not found", selectedNode.Name)
	}
	return selectedCSINode, nil
}

// aggregateTopologies returns all the supported topology values in the cluster that
// match the driver's topology keys.
func aggregateTopologies(
	driverName string,
	selectedCSINode *storagev1.CSINode,
	csiNodeLister storagelistersv1.CSINodeLister,
	nodeLister corelisters.NodeLister) ([]topologyTerm, error) {

	// 1. Determine topologyKeys to use for aggregation
	var topologyKeys []string
	if selectedCSINode == nil {
		// Immediate binding
		csiNodes, err := csiNodeLister.List(labels.Everything())
		if err != nil {
			// Require CSINode beta feature on K8s apiserver to be enabled.
			// We don't want to fallback and provision in the wrong topology if there's some temporary
			// error with the API server.
			return nil, fmt.Errorf("error listing CSINodes: %v", err)
		}
		rand.Shuffle(len(csiNodes), func(i, j int) {
			csiNodes[i], csiNodes[j] = csiNodes[j], csiNodes[i]
		})

		// Pick the first node with topology keys
		for _, csiNode := range csiNodes {
			topologyKeys = getTopologyKeys(csiNode, driverName)
			if topologyKeys != nil {
				break
			}
		}

		if len(topologyKeys) == 0 {
			// The driver supports topology but no nodes have registered any topology keys.
			// This is possible if nodes have not been upgraded to use the beta CSINode feature.
			klog.Warningf("No topology keys found on any node")
			return nil, nil
		}

	} else {
		// Delayed binding; use topology key from selected node
		topologyKeys = getTopologyKeys(selectedCSINode, driverName)
		if len(topologyKeys) == 0 {
			// The scheduler selected a node with no topology information.
			// This can happen if:
			//
			// * the node driver is not deployed on all nodes.
			// * the node driver is being restarted and has not re-registered yet. This should be
			//   temporary and a retry should eventually succeed.
			//
			// Returning an error in provisioning will cause the scheduler to retry and potentially
			// (but not guaranteed) pick a different node.
			return nil, fmt.Errorf("no topology key found on CSINode %s", selectedCSINode.Name)
		}

		// Even though selectedNode is set, we still need to aggregate topology values across
		// all nodes in order to find additional topologies for the volume types that can span
		// multiple topology values.
		//
		// TODO (#221): allow drivers to limit the number of topology values that are returned
		// If the driver specifies 1, then we can optimize here to only return the selected node's
		// topology instead of aggregating across all Nodes.
	}

	// 2. Find all nodes with the topology keys and extract the topology values
	selector, err := buildTopologyKeySelector(topologyKeys)
	if err != nil {
		return nil, err
	}
	nodes, err := nodeLister.List(selector)
	if err != nil {
		return nil, fmt.Errorf("error listing nodes: %v", err)
	}

	var terms []topologyTerm
	for _, node := range nodes {
		term, _ := getTopologyFromNode(node, topologyKeys)
		terms = append(terms, term)
	}
	if len(terms) == 0 {
		// This means that a CSINode was found with topologyKeys, but we couldn't find
		// the topology labels on any nodes.
		return nil, fmt.Errorf("topologyKeys %v were not found on any nodes", topologyKeys)
	}
	return terms, nil
}

// AllowedTopologies is an OR of TopologySelectorTerms.
// A TopologySelectorTerm contains an AND of TopologySelectorLabelRequirements.
// A TopologySelectorLabelRequirement contains a single key and an OR of topology values.
//
// The Requisite field contains an OR of Segments.
// A segment contains an AND of topology key value pairs.
//
// In order to convert AllowedTopologies to CSI Requisite, one of its OR layers must be eliminated.
// This function eliminates the OR of topology values by distributing the OR over the AND a level
// higher.
// For example, given a TopologySelectorTerm of this form:
//    {
//      "zone": { "zone1", "zone2" },
//      "rack": { "rackA", "rackB" },
//    }
// Abstractly it could be viewed as:
//    (zone1 OR zone2) AND (rackA OR rackB)
// Distributing the OR over the AND, we get:
//    (zone1 AND rackA) OR (zone2 AND rackA) OR (zone1 AND rackB) OR (zone2 AND rackB)
// which in the intermediate representation returned by this function becomes:
//    [
//      { "zone": "zone1", "rack": "rackA" },
//      { "zone": "zone2", "rack": "rackA" },
//      { "zone": "zone1", "rack": "rackB" },
//      { "zone": "zone2", "rack": "rackB" },
//    ]
//
// This flattening is then applied to all TopologySelectorTerms in AllowedTopologies, and
// the resulting terms are OR'd together.
func flatten(allowedTopologies []v1.TopologySelectorTerm) []topologyTerm {
	var finalTerms []topologyTerm
	for _, selectorTerm := range allowedTopologies { // OR

		var oldTerms []topologyTerm
		for _, selectorExpression := range selectorTerm.MatchLabelExpressions { // AND

			var newTerms []topologyTerm
			for _, v := range selectorExpression.Values { // OR
				// Distribute the OR over AND.

				if len(oldTerms) == 0 {
					// No previous terms to distribute over. Simply append the new term.
					newTerms = append(newTerms, topologyTerm{selectorExpression.Key: v})
				} else {
					for _, oldTerm := range oldTerms {
						// "Distribute" by adding an entry to the term
						newTerm := oldTerm.clone()
						newTerm[selectorExpression.Key] = v
						newTerms = append(newTerms, newTerm)
					}
				}
			}

			oldTerms = newTerms
		}

		// Concatenate all OR'd terms.
		finalTerms = append(finalTerms, oldTerms...)
	}

	return finalTerms
}

func deduplicate(terms []topologyTerm) []topologyTerm {
	termMap := make(map[string]topologyTerm)
	for _, term := range terms {
		termMap[term.hash()] = term
	}

	var dedupedTerms []topologyTerm
	for _, term := range termMap {
		dedupedTerms = append(dedupedTerms, term)
	}
	return dedupedTerms
}

// Sort the given terms in place,
// then return a new list of terms equivalent to the sorted terms, but shifted so that
// either the primary term (if specified) or term at shiftIndex is the first in the list.
func sortAndShift(terms []topologyTerm, primary topologyTerm, shiftIndex uint32) []topologyTerm {
	var preferredTerms []topologyTerm
	sort.Slice(terms, func(i, j int) bool {
		return terms[i].less(terms[j])
	})
	if primary == nil {
		preferredTerms = append(terms[shiftIndex:], terms[:shiftIndex]...)
	} else {
		for i, t := range terms {
			if t.subset(primary) {
				preferredTerms = append(terms[i:], terms[:i]...)
				break
			}
		}
	}
	return preferredTerms
}

func getTopologyKeys(csiNode *storagev1.CSINode, driverName string) []string {
	for _, driver := range csiNode.Spec.Drivers {
		if driver.Name == driverName {
			return driver.TopologyKeys
		}
	}
	return nil
}

func getTopologyFromNode(node *v1.Node, topologyKeys []string) (term topologyTerm, isMissingKey bool) {
	term = make(topologyTerm)
	for _, key := range topologyKeys {
		v, ok := node.Labels[key]
		if !ok {
			return nil, true
		}
		term[key] = v
	}
	return term, false
}

func buildTopologyKeySelector(topologyKeys []string) (labels.Selector, error) {
	var expr []metav1.LabelSelectorRequirement
	for _, key := range topologyKeys {
		expr = append(expr, metav1.LabelSelectorRequirement{
			Key:      key,
			Operator: metav1.LabelSelectorOpExists,
		})
	}

	labelSelector := metav1.LabelSelector{
		MatchExpressions: expr,
	}

	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, fmt.Errorf("error parsing topology keys selector: %v", err)
	}

	return selector, nil
}

func (t topologyTerm) clone() topologyTerm {
	ret := make(topologyTerm)
	for k, v := range t {
		ret[k] = v
	}
	return ret
}

// "<k1>#<v1>,<k2>#<v2>,..."
// Hash properties:
// - Two equivalent topologyTerms have the same hash
// - Ordering of hashes correspond to a natural ordering of their topologyTerms. For example:
//   - com.example.csi/zone#zone1 < com.example.csi/zone#zone2
//   - com.example.csi/rack#zz    < com.example.csi/zone#zone1
//   - com.example.csi/z#z1       < com.example.csi/zone#zone1
//   - com.example.csi/rack#rackA,com.example.csi/zone#zone2  <  com.example.csi/rackB,com.example.csi/zone#zone1
//   Note that both '#' and ',' are less than '/', '-', '_', '.', [A-Z0-9a-z]
func (t topologyTerm) hash() string {
	var segments []string
	for k, v := range t {
		segments = append(segments, k+"#"+v)
	}

	sort.Strings(segments)
	return strings.Join(segments, ",")
}

func (t topologyTerm) less(other topologyTerm) bool {
	return t.hash() < other.hash()
}

func (t topologyTerm) subset(other topologyTerm) bool {
	for key, tv := range t {
		v, ok := other[key]
		if !ok || v != tv {
			return false
		}
	}

	return true
}

func (t topologyTerm) equal(other topologyTerm) bool {
	return t.hash() == other.hash()
}

func toCSITopology(terms []topologyTerm) []*csi.Topology {
	var out []*csi.Topology
	for _, term := range terms {
		out = append(out, &csi.Topology{Segments: term})
	}
	return out
}

// identical to logic in getPVCNameHashAndIndexOffset in pkg/volume/util/util.go in-tree
// [https://github.com/kubernetes/kubernetes/blob/master/pkg/volume/util/util.go]
func getPVCNameHashAndIndexOffset(pvcName string) (hash uint32, index uint32) {
	if pvcName == "" {
		// We should always be called with a name; this shouldn't happen
		hash = rand.Uint32()
	} else {
		hashString := pvcName

		// Heuristic to make sure that volumes in a StatefulSet are spread across zones
		// StatefulSet PVCs are (currently) named ClaimName-StatefulSetName-Id,
		// where Id is an integer index.
		// Note though that if a StatefulSet pod has multiple claims, we need them to be
		// in the same zone, because otherwise the pod will be unable to mount both volumes,
		// and will be unschedulable.  So we hash _only_ the "StatefulSetName" portion when
		// it looks like `ClaimName-StatefulSetName-Id`.
		// We continue to round-robin volume names that look like `Name-Id` also; this is a useful
		// feature for users that are creating statefulset-like functionality without using statefulsets.
		lastDash := strings.LastIndexByte(pvcName, '-')
		if lastDash != -1 {
			statefulsetIDString := pvcName[lastDash+1:]
			statefulsetID, err := strconv.ParseUint(statefulsetIDString, 10, 32)
			if err == nil {
				// Offset by the statefulsetID, so we round-robin across zones
				index = uint32(statefulsetID)
				// We still hash the volume name, but only the prefix
				hashString = pvcName[:lastDash]

				// In the special case where it looks like `ClaimName-StatefulSetName-Id`,
				// hash only the StatefulSetName, so that different claims on the same StatefulSet
				// member end up in the same zone.
				// Note that StatefulSetName (and ClaimName) might themselves both have dashes.
				// We actually just take the portion after the final - of ClaimName-StatefulSetName.
				// For our purposes it doesn't much matter (just suboptimal spreading).
				lastDash := strings.LastIndexByte(hashString, '-')
				if lastDash != -1 {
					hashString = hashString[lastDash+1:]
				}
			}
		}

		// We hash the (base) volume name, so we don't bias towards the first N zones
		h := fnv.New32()
		h.Write([]byte(hashString))
		hash = h.Sum32()
	}

	return hash, index
}
