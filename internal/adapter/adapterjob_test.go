package adapter

import (
	"reflect"
	"testing"
)

// The tool scope the adapters once took is gone from the interface: a job
// carries a permission value and no Tools, and no adapter is asked for a
// shell scope.
func TestJobCarriesPermissionNotToolScope(t *testing.T) {
	typ := reflect.TypeOf(Job{})
	if _, ok := typ.FieldByName("Tools"); ok {
		t.Error("Job still has a Tools field")
	}
	if f, ok := typ.FieldByName("Permission"); !ok || f.Type.Kind() != reflect.String {
		t.Error("Job lacks a string Permission field")
	}
}
