package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestTaintsEqual(t *testing.T) {
	a := []corev1.Taint{
		{Key: "dedicated", Value: "workers", Effect: corev1.TaintEffectNoSchedule},
		{Key: "spot", Effect: corev1.TaintEffectPreferNoSchedule},
	}
	b := []corev1.Taint{
		{Key: "spot", Effect: corev1.TaintEffectPreferNoSchedule},
		{Key: "dedicated", Value: "workers", Effect: corev1.TaintEffectNoSchedule},
	}
	if !taintsEqual(a, b) {
		t.Fatal("taint sets with the same members should be equal regardless of order")
	}
	if taintsEqual(a, nil) {
		t.Fatal("non-empty and empty taint sets must differ")
	}
	c := []corev1.Taint{{Key: "dedicated", Value: "other", Effect: corev1.TaintEffectNoSchedule}}
	if taintsEqual(a, c) {
		t.Fatal("different taint values must differ")
	}
}
