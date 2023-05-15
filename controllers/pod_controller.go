/*
Copyright 2023.

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

package controllers

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PodReconciler reconciles a Pod object
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Pod object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
// Reconcile has to watch the pod object if it has the scheduling gate with name schedulingGateName,
// inspect the images in the pod spec, update the nodeAffinity accordingly and remove the scheduling gate.
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		klog.V(3).Infof("unable to fetch Pod %s/%s: %v", req.Namespace, req.Name, err)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// verify whether the pod is in the proper phase to add a schedulingGate

	// verify whether the pod has the scheduling gate
	if !hasSchedulingGate(pod) {
		klog.V(4).Infof("pod %s/%s does not have the scheduling gate. Ignoring...", pod.Namespace, pod.Name)
		// if not, return
		return ctrl.Result{}, nil
	}
	klog.Infof("Processing pod %s/%s", pod.Namespace, pod.Name)
	// The scheduling gate is found.
	var err error
	// Prepare the requirement for the node affinity.
	architectureRequirement, err := prepareRequirement(ctx, pod)
	if err != nil {
		klog.Errorf("unable to prepare the requirement for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return ctrl.Result{}, err
	}

	// Update the node affinity
	err = setPodNodeAffinityRequirement(ctx, pod, architectureRequirement)
	if err != nil {
		klog.Errorf("unable to set the node affinity requirement for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return ctrl.Result{}, err
	}

	// Remove the scheduling gate
	// klog.Infof("Reomving the scheduling gate from pod %s/%s", pod.Namespace, pod.Name)
	// removeSchedulingGate(pod)

	jsonData, err := json.Marshal(pod)
	if err != nil {
		fmt.Println("Error marshaling resource:", err)
	}

	// Print the JSON data
	fmt.Println(string(jsonData))
	err = r.Client.Update(ctx, pod)
	if err != nil {
		klog.Errorf("unable to update the pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func prepareRequirement(ctx context.Context, pod *corev1.Pod) (corev1.NodeSelectorRequirement, error) {
	// TODO: inspect the images in the pod spec to infer the values
	return corev1.NodeSelectorRequirement{
		Key:      "kubernetes.io/architecture",
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{"amd64", "arm64", "s390x", "ppc64le"},
	}, nil
}

// setPodNodeAffinityRequirement sets the node affinity for the pod to the given requirement based on the rules in
// the sig-scheduling's KEP-3838: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/3838-pod-mutable-scheduling-directives.
func setPodNodeAffinityRequirement(ctx context.Context, pod *corev1.Pod,
	requirement corev1.NodeSelectorRequirement) error {
	// We are ignoring the podSpec.nodeSelector field,
	// TODO: validate this is ok when a pod has both nodeSelector and (our) nodeAffinity.
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}

	// the .requiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms are ORed
	if len(pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		// We create a new array of NodeSelectorTerm of length one so that we can always iterate it in the next.
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = make([]corev1.NodeSelectorTerm, 1)
	}
	nodeSelectorTerms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	// The expressions within the nodeSelectorTerms are ANDed.
	// Therefore, we iterate over the nodeSelectorTerms and add an expression to each of the terms to verify the
	// kubernetes.io/architecture label has compatible values.
	// Note that the NodeSelectorTerms will always be long at least 1, because we (re-)created it with size 1 above if it was nil (or having 0 length).
	for i := range nodeSelectorTerms {
		if nodeSelectorTerms[i].MatchExpressions == nil {
			nodeSelectorTerms[i].MatchExpressions = make([]corev1.NodeSelectorRequirement, 0, 1)
		}
		// Check if the nodeSelectorTerm already has a matchExpression for the kubernetes.io/architecture label.
		// if yes, we ignore to add it.
		for _, expression := range nodeSelectorTerms[i].MatchExpressions {
			if expression.Key == requirement.Key {
				klog.V(4).Infof("a nodeSelectorTerm already has the matchExpression for the kubernetes.io/architecture label. Ignoring...")
				continue
			}
		}
		// When reaching here, we know that the nodeSelectorTerm does not have the matchExpression for the kubernetes.io/architecture label.
		// We add it.
		nodeSelectorTerms[i].MatchExpressions = append(nodeSelectorTerms[i].MatchExpressions, requirement)
		fmt.Println(nodeSelectorTerms[i])

	}
	return nil
}

func hasSchedulingGate(pod *corev1.Pod) bool {
	if pod.Spec.SchedulingGates == nil {
		// If the schedulingGates array is nil, we return false
		return false
	}
	for _, condition := range pod.Spec.SchedulingGates {
		if condition.Name == schedulingGateName {
			return true
		}
	}
	// the scheduling gate is not found.
	return false
}

func removeSchedulingGate(pod *corev1.Pod) {
	if len(pod.Spec.SchedulingGates) == 0 {
		// If the schedulingGates array is nil, we return
		return
	}
	filtered := make([]corev1.PodSchedulingGate, 0, len(pod.Spec.SchedulingGates))
	for _, schedulingGate := range pod.Spec.SchedulingGates {
		if schedulingGate.Name != schedulingGateName {
			filtered = append(filtered, schedulingGate)
		}
	}
	pod.Spec.SchedulingGates = filtered
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
