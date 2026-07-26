package main

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "quota.melonlorrd.dev", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&ObjectQuota{},
		&ObjectQuotaList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

type ObjectQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ObjectQuotaSpec   `json:"spec,omitempty"`
	Status            ObjectQuotaStatus `json:"status,omitempty"`
}

type ObjectQuotaSpec struct {
	FileDescriptors *int64 `json:"fileDescriptors,omitempty"`
	Processes       *int64 `json:"processes,omitempty"`
	LeaseBatchSize  *int64 `json:"leaseBatchSize,omitempty"`
}

type ObjectQuotaStatus struct {
	FDAllocations   map[string]int64 `json:"fdAllocations,omitempty"`
	ProcAllocations map[string]int64 `json:"procAllocations,omitempty"`
	AvailableFD     int64            `json:"available,omitempty"`
	Version         *int64           `json:"version,omitempty"`
}

type ObjectQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectQuota   `json:"items"`
}

func (in *ObjectQuota) DeepCopyInto(out *ObjectQuota) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ObjectQuota) DeepCopy() *ObjectQuota {
	if in == nil {
		return nil
	}
	out := new(ObjectQuota)
	in.DeepCopyInto(out)
	return out
}

func (in *ObjectQuota) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *ObjectQuotaList) DeepCopyInto(out *ObjectQuotaList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]ObjectQuota, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *ObjectQuotaList) DeepCopy() *ObjectQuotaList {
	if in == nil {
		return nil
	}
	out := new(ObjectQuotaList)
	in.DeepCopyInto(out)
	return out
}

func (in *ObjectQuotaList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *ObjectQuotaSpec) DeepCopyInto(out *ObjectQuotaSpec) {
	*out = *in
	if in.FileDescriptors != nil {
		in, out := &in.FileDescriptors, &out.FileDescriptors
		*out = new(int64)
		**out = **in
	}
	if in.Processes != nil {
		in, out := &in.Processes, &out.Processes
		*out = new(int64)
		**out = **in
	}
	if in.LeaseBatchSize != nil {
		in, out := &in.LeaseBatchSize, &out.LeaseBatchSize
		*out = new(int64)
		**out = **in
	}
}

func (in *ObjectQuotaSpec) DeepCopy() *ObjectQuotaSpec {
	if in == nil {
		return nil
	}
	out := new(ObjectQuotaSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ObjectQuotaStatus) DeepCopyInto(out *ObjectQuotaStatus) {
	*out = *in
	if in.FDAllocations != nil {
		in, out := &in.FDAllocations, &out.FDAllocations
		*out = make(map[string]int64, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.ProcAllocations != nil {
		in, out := &in.ProcAllocations, &out.ProcAllocations
		*out = make(map[string]int64, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.Version != nil {
		in, out := &in.Version, &out.Version
		*out = new(int64)
		**out = **in
	}
}

func (in *ObjectQuotaStatus) DeepCopy() *ObjectQuotaStatus {
	if in == nil {
		return nil
	}
	out := new(ObjectQuotaStatus)
	in.DeepCopyInto(out)
	return out
}
