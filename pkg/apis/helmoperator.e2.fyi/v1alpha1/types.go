/*
Copyright 2019 eterna2
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    https://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Chart describes the helm chart to deploy.
type Chart string

// Release describes the helm release for the provided chart.
type Release string

// Namespace describes the k8s namespace to deploy the helm chart.
type Namespace string

// Revision describes the release revision (usually for rollback).
type Revision int64

// Values describes the values to apply on the helm chart.
type Values interface{}

// TLSSecret is .
type TLSSecret struct {
	Key string `json:"key,omitempty"`
	Crt string `json:"crt,omitempty"`
}

// HelmOperationType describes the helm operation, i.e. install, upgrade, or
// rollback
type HelmOperationType string

// different types of helm operations
const (
	Install  HelmOperationType = "install"
	Upgrade  HelmOperationType = "upgrade"
	Rollback HelmOperationType = "rollback"
	Delete   HelmOperationType = "delete"
)

// HelmOperationStatus describes the current status of a helm operation.
type HelmOperationStatus struct {
	// Message is the helm stdout message.
	Message string `json: "message,omitempty"`
	// Revision is the current revision of the helm release.
	Revision int64 `json:"submissionID,omitempty"`
	// CompletionTime is the time when the application runs to completion if it does.
	TerminationTime metav1.Time `json:"terminationTime,omitempty"`
}

// HelmOperationSpec describes the specifications to deploy, upgrade, or
// rollback a helm chart.
type HelmOperationSpec struct {
	Chart     Chart     `json:"chart"`
	Release   Release   `json:"release"`
	Revision  Revision  `json:"revision,omitempty"`
	Namespace Namespace `json:"namespace"`
	Values    Values    `json:"values,omitempty"`
	TLSSecret TLSSecret `json:"tls,omitempty"`
}

// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// HelmOperation represents a helm operation in the k8s cluster.
type HelmOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              HelmOperationSpec   `json:"spec"`
	Status            HelmOperationStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// HelmOperationList carries a list of HelmOperation objects.
type HelmOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HelmOperation `json:"items,omitempty"`
}
