package controllers

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/json"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"strings"
)

const (
	// SchedulingGateName is the name of the Scheduling Gate
	schedulingGateName = "multi-arch.openshift.io/scheduling-gate"
)

var schedulingGate = corev1.PodSchedulingGate{
	Name: schedulingGateName,
}

// +kubebuilder:webhook:path=/add-pod-scheduling-gate,mutating=true,sideEffects=None,admissionReviewVersions=v1,failurePolicy=ignore,groups="",resources=pods,verbs=create,versions=v1,name=arch-aware.scheduling-gate.openshift.io
// TODO: failurePolicy?

// PodSchedulingGateMutatingWebHook annotates Pods
type PodSchedulingGateMutatingWebHook struct {
	Client  client.Client
	decoder *admission.Decoder
}

func (a *PodSchedulingGateMutatingWebHook) InjectDecoder(d *admission.Decoder) error {
	a.decoder = d
	return nil
}

func (a *PodSchedulingGateMutatingWebHook) patchedPodResponse(pod *corev1.Pod, req admission.Request) admission.Response {
	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

func (a *PodSchedulingGateMutatingWebHook) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	err := a.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// ignore the openshift-* namespace as those are infra components
	if strings.HasPrefix(pod.Namespace, "openshift-") {
		return a.patchedPodResponse(pod, req)
	}
	// https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/3521-pod-scheduling-readiness
	if pod.Spec.SchedulingGates == nil {
		pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{}
	} else {
		//if gate is already present, do not continue - is this even possible?
		for _, schedulingGate := range pod.Spec.SchedulingGates {
			if schedulingGate.Name == schedulingGateName {
				return a.patchedPodResponse(pod, req)
			}
		}
	}

	pod.Spec.SchedulingGates = append(pod.Spec.SchedulingGates, schedulingGate)

	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
		// fix a bug in the kube-apiserver validation API
	}
	return a.patchedPodResponse(pod, req)
}
