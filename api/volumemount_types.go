// Package api declares the resources this agent owns.
//
// Only one so far, and it lives here rather than in the platform's api package
// on purpose: a host directory is something only a machine-backed location can
// give a runtime. A Kubernetes location has no host to bind from, so the kind
// would be one the platform's own controller could never act on.
package api

import (
	"github.com/reconcile-kit/api/conditions"
	"github.com/reconcile-kit/api/resource"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
)

const (
	VolumeMountKind  = "volume-mount"
	VolumeMountGroup = platform.RuntimeGroup

	VolumeMountCond = "VolumeMount"
)

// VolumeMount binds a directory on the machine into a runtime's container.
//
// It is a resource of its own rather than a field on the Runtime because the
// two have different lifetimes: a directory is shared with a box for as long as
// someone is working on it, and taking it away should not mean editing — and
// risking — the description of the box itself.
//
// The shard is the location's, like the Runtime it points at: the mount can
// only be made by the agent that owns that machine.
type VolumeMount struct {
	resource.Resource
	Spec   VolumeMountSpec   `json:"spec"`
	Status VolumeMountStatus `json:"status"`
}

type VolumeMountSpec struct {
	// RuntimeRef is the box to mount into. It is resolved in this resource's
	// own namespace — a mount for someone else's box is not a thing to express.
	RuntimeRef platform.LocalObjectRef `json:"runtimeRef"`
	// HostPath is the directory on the machine. Absolute, and it has to exist:
	// docker would otherwise create it, root-owned, and a typo would look like
	// an empty directory rather than a mistake.
	HostPath string `json:"hostPath"`
	// ContainerPath is where it appears inside the box. Absolute.
	ContainerPath string `json:"containerPath"`
	// ReadOnly binds it without write access.
	ReadOnly bool `json:"readOnly,omitempty"`
}

type VolumeMountStatus struct {
	ObservedGeneration int                    `json:"observedGeneration"`
	Phase              platform.Phase         `json:"phase"`
	Conditions         []conditions.Condition `json:"conditions"`
}

func (c *VolumeMount) GetConditions() []conditions.Condition {
	return c.Status.Conditions
}

func (c *VolumeMount) SetConditions(i []conditions.Condition) {
	c.Status.Conditions = i
}

func (c *VolumeMount) GetGK() resource.GroupKind {
	return resource.GroupKind{Kind: VolumeMountKind, Group: VolumeMountGroup}
}

func (c *VolumeMount) DeepCopy() *VolumeMount {
	return resource.DeepCopyStruct(c).(*VolumeMount)
}
