package adapter

import (
	"reflect"
	"testing"
)

// TestJobCarriesPermissionNotAToolScope: the job hands the harness the
// agent's resolved permission value; the tool scope it replaced is gone.
func TestJobCarriesPermissionNotAToolScope(t *testing.T) {
	typ := reflect.TypeOf(Job{})
	if f, ok := typ.FieldByName("Permission"); !ok || f.Type.Kind() != reflect.String {
		t.Errorf("Job has no string Permission field")
	}
	if _, ok := typ.FieldByName("Tools"); ok {
		t.Errorf("Job still carries the retired Tools scope")
	}
}
