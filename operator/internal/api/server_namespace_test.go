package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

// The API serves the scenarios of the namespace it is given (the operator's
// own), so the lab can run in a namespace other than "default".
func TestServerUsesItsNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&v1alpha1.MaintenanceJob{ObjectMeta: metav1.ObjectMeta{Name: "here", Namespace: "rca-lab"}},
		&v1alpha1.MaintenanceJob{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "default"}},
	).Build()
	h := NewServer(c, "", "rca-lab").routes()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get("/api/scenarios")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d: %s", rec.Code, rec.Body)
	}
	var list []scenarioSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "here" {
		t.Fatalf("list: want only %q, got %+v", "here", list)
	}

	if rec := get("/api/scenarios/here"); rec.Code != http.StatusOK {
		t.Fatalf("get here: status %d: %s", rec.Code, rec.Body)
	}
	if rec := get("/api/scenarios/elsewhere"); rec.Code != http.StatusNotFound {
		t.Fatalf("get elsewhere: want 404, got %d", rec.Code)
	}
}
