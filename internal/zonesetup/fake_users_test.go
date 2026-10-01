//nolint:goconst,varnamelen // The double repeats API field names and ids so each expectation reads on its own.
package zonesetup_test

import (
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

// routeUserAccess serves the users, credentials, policies and policy sets
// the per-user steps read and write.
func (f *zoneAPI) routeUserAccess(mux *http.ServeMux) {
	f.routeCredentialsAndUsers(mux)
	f.routePolicies(mux)
	f.routePolicySets(mux)
}

func (f *zoneAPI) routeCredentialsAndUsers(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones/{z}/application-credentials", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.page(w, filterRecords(f.credentials, func(c *record) bool {
			return matchQuery(q.Get("applicationId"), (*c)["application_id"])
		}))
	})
	mux.HandleFunc("POST /zones/{z}/application-credentials", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		c := &record{
			"id": f.id("cred"), "application_id": body["application_id"], "provider_id": body["provider_id"],
			"type": body["type"], "subject": body["subject"], "identifier": body["subject"],
		}
		f.credentials = append(f.credentials, c)
		f.write(w, http.StatusCreated, c)
	})
	mux.HandleFunc("GET /zones/{z}/users", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.page(w, filterRecords(f.users, func(u *record) bool {
			return matchQuery(q.Get("filter[email]"), (*u)["email"])
		}))
	})
}

func (f *zoneAPI) routePolicies(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones/{z}/policy-schemas", func(w http.ResponseWriter, _ *http.Request) {
		var items []*record
		if f.schemaVersion != "" {
			items = append(items, &record{"version": f.schemaVersion, "is_default": true, "status": "active"})
		}
		f.cursorPage(w, items)
	})
	mux.HandleFunc("GET /zones/{z}/policies", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.cursorPage(w, filterRecords(f.policies, func(p *record) bool {
			return containsQuery(q["query[name]"], (*p)["name"])
		}))
	})
	mux.HandleFunc("POST /zones/{z}/policies", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		p := &record{
			"id": f.id("pol"), "name": body["name"], "description": body["description"],
			"owner_type": "customer", "archived_at": nil,
		}
		f.policies = append(f.policies, p)
		f.write(w, http.StatusCreated, p)
	})
	mux.HandleFunc("GET /zones/{z}/policies/{p}/versions", func(w http.ResponseWriter, r *http.Request) {
		items := filterRecords(f.policyVers, func(v *record) bool { return (*v)["policy_id"] == r.PathValue("p") })
		// Newest first, as the API's default order.
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
		if r.URL.Query().Get("limit") == "1" && len(items) > 1 {
			items = items[:1]
		}
		f.cursorPage(w, items)
	})
	mux.HandleFunc("POST /zones/{z}/policies/{p}/versions", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		if body["schema_version"] != f.schemaVersion {
			f.write(w, http.StatusBadRequest, record{"error": "unknown schema_version"})
			return
		}
		n := f.count(f.policyVers, "policy_id", r.PathValue("p")) + 1
		v := &record{
			"id": f.id("pv"), "policy_id": r.PathValue("p"), "version": n, "cedar_raw": body["cedar_raw"],
			"schema_version": body["schema_version"], "sha": fmt.Sprintf("sha-%d", n),
		}
		f.policyVers = append(f.policyVers, v)
		f.write(w, http.StatusCreated, v)
	})
}

func (f *zoneAPI) routePolicySets(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones/{z}/policy-sets", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var items []*record
		for _, s := range f.sets {
			view := f.setView(s)
			if !containsQuery(q["query[name]"], view["name"]) {
				continue
			}
			if q.Get("filter[active]") == "true" && view["active"] != true {
				continue
			}
			items = append(items, &view)
		}
		f.cursorPage(w, items)
	})
	mux.HandleFunc("POST /zones/{z}/policy-sets", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		s := &record{"id": f.id("set"), "name": body["name"], "target_type": body["target_type"], "owner_type": "customer"}
		f.sets = append(f.sets, s)
		f.write(w, http.StatusCreated, f.setView(s))
	})
	mux.HandleFunc("GET /zones/{z}/policy-sets/{s}/versions/{v}", func(w http.ResponseWriter, r *http.Request) {
		v := f.find(f.setVers, r.PathValue("v"))
		if v == nil || (*v)["policy_set_id"] != r.PathValue("s") {
			f.write(w, http.StatusNotFound, nil)
			return
		}
		f.write(w, http.StatusOK, f.setVersionView(v))
	})
	mux.HandleFunc("POST /zones/{z}/policy-sets/{s}/versions", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		if body["schema_version"] != f.schemaVersion {
			f.write(w, http.StatusBadRequest, record{"error": "unknown schema_version"})
			return
		}
		n := f.count(f.setVers, "policy_set_id", r.PathValue("s")) + 1
		v := &record{
			"id": f.id("sv"), "policy_set_id": r.PathValue("s"), "version": n,
			"manifest": body["manifest"], "schema_version": body["schema_version"],
		}
		f.setVers = append(f.setVers, v)
		f.write(w, http.StatusCreated, f.setVersionView(v))
	})
	mux.HandleFunc("PATCH /zones/{z}/policy-sets/{s}/versions/{v}", f.activatePolicySetVersion)
}

func (f *zoneAPI) activatePolicySetVersion(w http.ResponseWriter, r *http.Request) {
	body := f.body(r)
	v := f.find(f.setVers, r.PathValue("v"))
	if v == nil || (*v)["policy_set_id"] != r.PathValue("s") {
		f.write(w, http.StatusNotFound, nil)
		return
	}
	if body["active"] != true {
		f.write(w, http.StatusBadRequest, record{"error": "active must be true"})
		return
	}
	id, _ := (*v)["id"].(string)
	f.activeVersion = id
	f.write(w, http.StatusOK, f.setVersionView(v))
}

func (f *zoneAPI) count(items []*record, key, value string) int {
	n := 0
	for _, it := range items {
		if (*it)[key] == value {
			n++
		}
	}
	return n
}

func (f *zoneAPI) setView(s *record) record {
	view := record{}
	maps.Copy(view, *s)
	view["active"] = false
	view["active_version_id"] = nil
	view["latest_version_id"] = nil
	var latest *record
	for _, v := range f.setVers {
		if (*v)["policy_set_id"] != (*s)["id"] {
			continue
		}
		latest = v
		if (*v)["id"] == f.activeVersion {
			view["active"] = true
			view["active_version_id"] = (*v)["id"]
			view["active_version"] = (*v)["version"]
		}
	}
	if latest != nil {
		view["latest_version_id"] = (*latest)["id"]
		view["latest_version"] = (*latest)["version"]
	}
	return view
}

func (f *zoneAPI) setVersionView(v *record) record {
	view := record{}
	maps.Copy(view, *v)
	view["active"] = (*v)["id"] == f.activeVersion
	return view
}

func (f *zoneAPI) cursorPage(w http.ResponseWriter, items []*record) {
	if items == nil {
		items = []*record{}
	}
	f.write(w, http.StatusOK, record{keyItems: items, keyPagination: record{keyAfterCursor: "", "before_cursor": ""}})
}

// seedOtherActiveSet gives the zone an active policy set that is not the
// sandbox set.
func (f *zoneAPI) seedOtherActiveSet() {
	f.sets = append(f.sets, &record{"id": "set-other", "name": "Production", "target_type": "zone"})
	f.setVers = append(f.setVers, &record{
		"id": "sv-other", "policy_set_id": "set-other", "version": firstVersion, "manifest": record{"entries": []any{}},
	})
	f.activeVersion = "sv-other"
}

// containsQuery is true when no term is given or any term is a substring of
// the value, as the API's query[] search is.
func containsQuery(terms []string, value any) bool {
	if len(terms) == 0 {
		return true
	}
	s, _ := value.(string)
	for _, t := range terms {
		if strings.Contains(strings.ToLower(s), strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// seedCompleteUsers adds the per-user entities a completed run leaves behind.
func (f *zoneAPI) seedCompleteUsers() {
	f.credentials = append(f.credentials, &record{
		"id": "cred-1", "application_id": "app-1", "provider_id": "prov-1", "type": "token",
		"subject": "system:serviceaccount:agents:usr-1", "identifier": "system:serviceaccount:agents:usr-1",
	})
	// Both halves of the user's decision, in the order ensureUsers writes
	// them: the actor policy first, then the subject grant.
	f.policies = append(f.policies,
		&record{"id": "pol-1", "name": zonesetup.PolicyName("usr-1"), "owner_type": "customer", "archived_at": nil},
		&record{"id": "pol-2", "name": zonesetup.SubjectGrantName("usr-1"), "owner_type": "customer", "archived_at": nil},
	)
	f.policyVers = append(f.policyVers,
		&record{
			"id": "pv-1", "policy_id": "pol-1", "version": firstVersion,
			"cedar_raw":      zonesetup.ImpersonationPolicy(zonesetup.DefaultNamespace, "usr-1"),
			"schema_version": testSchemaVersion,
		},
		&record{
			"id": "pv-2", "policy_id": "pol-2", "version": firstVersion,
			"cedar_raw":      zonesetup.SubjectGrantPolicy("usr-1", testResourceIdentifiers()),
			"schema_version": testSchemaVersion,
		},
	)
	f.sets = append(f.sets, &record{
		"id": "set-1", "name": zonesetup.DefaultPolicySetName, "target_type": "zone", "owner_type": "customer",
	})
	f.setVers = append(f.setVers, &record{
		"id": "sv-1", "policy_set_id": "set-1", "version": firstVersion, "schema_version": testSchemaVersion,
		"manifest": record{"entries": []any{
			record{"policy_id": "pol-1", "policy_version_id": "pv-1"},
			record{"policy_id": "pol-2", "policy_version_id": "pv-2"},
		}},
	})
	f.activeVersion = "sv-1"
	f.counter["cred"], f.counter["pol"], f.counter["pv"], f.counter["set"], f.counter["sv"] = 1, 2, 2, 1, 1
}
