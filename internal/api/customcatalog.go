package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Daniel-Vaz/KaaS-demo/internal/app"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// customcatalog.go serves the per-user custom add-on catalogs (see internal/app/customcatalog.go).
// All routes require a session (registered inside the authenticated group in Routes). Errors map the
// same way as clusters: store.ErrNotFound → 404 (invisible catalog), app.ErrForbidden → 403 (read-only
// group member), otherwise 400 for validation.

func (s *Server) listCustomCatalogs(w http.ResponseWriter, r *http.Request) {
	views, err := s.app.ListCustomCatalogs(actorFrom(r))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) createCustomCatalog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cc, err := s.app.CreateCustomCatalog(actorFrom(r), req.Name)
	if err != nil {
		if errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, cc)
}

func (s *Server) getCustomCatalog(w http.ResponseWriter, r *http.Request) {
	view, err := s.app.GetCustomCatalog(actorFrom(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) renameCustomCatalog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cc, err := s.app.RenameCustomCatalog(actorFrom(r), r.PathValue("id"), req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, cc)
}

func (s *Server) deleteCustomCatalog(w http.ResponseWriter, r *http.Request) {
	if err := s.app.DeleteCustomCatalog(actorFrom(r), r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addCustomAddon(w http.ResponseWriter, r *http.Request) {
	var addon domain.CustomAddon
	if err := json.NewDecoder(r.Body).Decode(&addon); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cc, err := s.app.AddCustomAddon(actorFrom(r), r.PathValue("id"), addon)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, cc)
}

func (s *Server) updateCustomAddon(w http.ResponseWriter, r *http.Request) {
	var addon domain.CustomAddon
	if err := json.NewDecoder(r.Body).Decode(&addon); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cc, err := s.app.UpdateCustomAddon(actorFrom(r), r.PathValue("id"), r.PathValue("name"), addon)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, cc)
}

func (s *Server) removeCustomAddon(w http.ResponseWriter, r *http.Request) {
	cc, err := s.app.RemoveCustomAddon(actorFrom(r), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, cc)
}

// fetchChartValues fetches a Helm chart's default values for the authoring editor - also validating
// the repo/chart/version (an unreachable chart surfaces as an error in real mode).
func (s *Server) fetchChartValues(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo    string `json:"repo"`
		Chart   string `json:"chart"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	vals, err := s.app.FetchChartValues(r.Context(), actorFrom(r), req.Repo, req.Chart, req.Version)
	if err != nil {
		if errors.Is(err, app.ErrForbidden) {
			writeErr(w, statusFor(err), err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"values": vals})
}
