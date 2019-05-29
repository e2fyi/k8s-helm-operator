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
type Values interface {
	DeepCopyValues() Values
}

// TLSSecret is .
type TLSSecret struct {
	Key string `json:"key,omitempty"`
	Crt string `json:"crt,omitempty"`
}

// HelmOperationStateType describes the state of the HelmOperation.
type OperationStateType string

// Different type of states for a HelmOperation.
const (
	NewState              OperationStateType = ""
	InvalidatingState     OperationStateType = "INVALIDATING"
	SubmittedState        OperationStateType = "SUBMITTED"
	FailedSubmissionState OperationStateType = "SUBMISSION_FAILED"
	RunningState          OperationStateType = "RUNNING"
	SucceedingState       OperationStateType = "SUCCEEDING"
	CompletedState        OperationStateType = "COMPLETED"
	FailingState          OperationStateType = "FAILING"
	FailedState           OperationStateType = "FAILED"
	UnknownState          OperationStateType = "UNKNOWN"
)

// ApplicationState tells the current state of the application and an error message in case of failures.
type OperationState struct {
	State        OperationStateType `json:"state"`
	ErrorMessage string             `json:"errorMessage"`
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
	// OpState is the current state of the helm operation
	OpState OperationState `json:"state,omitempty"`
	// Message is the helm stdout message.
	Message string `json:"message,omitempty"`
	// Revision is the current revision of the helm release.
	Revision int64 `json:"submissionID,omitempty"`
	// CompletionTime is the time when the application runs to completion if it does.
	TerminationTime metav1.Time `json:"terminationTime,omitempty"`
}

// RepoName describes the name of the helm repo to add or update.
type RepoName string

// RepoURL describes the URL of the helm repo to add or update.
type RepoURL string

// HelmRepoSpec describes the specifications for the helm repo to add or update.
type HelmRepoSpec struct {
	Name RepoName `json:"name"`
	URL  RepoURL  `json:"url"`
}

// HelmOperationSpec describes the specifications to deploy, upgrade, or
// rollback a helm chart.
type HelmOperationSpec struct {
	Chart     Chart        `json:"chart"`
	Repo      HelmRepoSpec `json:"repo,omitempty"`
	Release   Release      `json:"release"`
	Revision  Revision     `json:"revision,omitempty"`
	Namespace Namespace    `json:"namespace"`
	Values    Values       `json:"values,omitempty"`
	TLSSecret TLSSecret    `json:"tls,omitempty"`
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
