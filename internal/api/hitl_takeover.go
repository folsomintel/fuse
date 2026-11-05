package api

import (
	"net/http"
	"strings"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/hostwire"
	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/go-chi/chi/v5"
)

// answerSecret answers a secret request (POST /v1/interventions/{id}?action=secret).
// the value goes into the guest and is dropped: it is never stored, logged,
// returned or put on the event stream, and the agent gets back "done".
// needs hitl:secret, which full-access keys do not hold.
func (h *Handler) answerSecret(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeSecret) {
		return
	}
	id := chi.URLParam(r, "interventionId")
	if !h.interventionVisible(w, r, id) {
		return
	}
	var req AnswerSecretRequest
	if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}
	in, err := h.Fleet.AnswerSecret(r.Context(), id, orchestrator.SecretAnswerRequest{
		Value:      req.Value,
		By:         principal(r).Actor,
		RemoteAddr: r.RemoteAddr,
	})
	if err != nil {
		writeFleetErrorRedacted(w, err, map[string]string{"value": req.Value})
		return
	}
	writeJSON(w, http.StatusOK, toAPIIntervention(in))
}

// startTakeover gives the caller control of an environment
// (POST /v1/environments/{vmId}?action=takeover). it claims the agent's
// pending takeover request if there is one, else opens one. while held, the
// agent's exec and computer calls get 423. needs hitl:takeover.
func (h *Handler) startTakeover(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeTakeover) {
		return
	}
	vmID := chi.URLParam(r, "vmId")
	if !h.covers(principal(r), vmID) {
		writeError(w, http.StatusNotFound, CodeNotFound, "environment not found", nil)
		return
	}
	var req TakeoverRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
			return
		}
	}
	in, err := h.Fleet.StartTakeover(r.Context(), vmID, orchestrator.TakeoverRequest{
		Via:        hitl.TakeoverVia(req.Via),
		Reason:     req.Reason,
		By:         principal(r).Actor,
		RemoteAddr: r.RemoteAddr,
	})
	if err != nil {
		writeHitlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIIntervention(in))
}

// releaseTakeover ends the held takeover and hands the note to the agent
// (POST /v1/environments/{vmId}?action=release). needs hitl:takeover.
func (h *Handler) releaseTakeover(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeTakeover) {
		return
	}
	vmID := chi.URLParam(r, "vmId")
	if !h.covers(principal(r), vmID) {
		writeError(w, http.StatusNotFound, CodeNotFound, "environment not found", nil)
		return
	}
	var req ReleaseRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, MaxJSONBodyBytes, &req) {
			return
		}
	}
	in, err := h.Fleet.ReleaseTakeover(r.Context(), vmID, req.Note, principal(r).Actor, r.RemoteAddr)
	if err != nil {
		writeHitlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIIntervention(in))
}

// takeoverAttach opens the takeover holder's shell
// (GET /v1/interventions/{id}/attach, Upgrade: fuse-attach/1). it is the
// attach stream with a recording on it; the session is saved as an
// encrypted cast and linked from a takeover_session audit row. unlike
// plain attach it is open to a hitl:takeover key, and only to the holder.
func (h *Handler) takeoverAttach(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeTakeover) {
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), hostwire.AttachProto) {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"takeover attach requires Upgrade: "+hostwire.AttachProto, nil)
		return
	}
	id := chi.URLParam(r, "interventionId")
	if !h.interventionVisible(w, r, id) {
		return
	}
	spec := hostwire.ParseAttachQuery(r.URL.Query())
	if !spec.TTY {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, "takeover attach requires tty=1", nil)
		return
	}
	p := principal(r)
	guest, err := h.Fleet.TakeoverAttach(r.Context(), id, spec, p.Actor, p.Master, r.RemoteAddr)
	if err != nil {
		writeHitlError(w, err)
		return
	}
	serveAttach(w, guest)
}

// takeoverDesktop opens the takeover holder's live desktop
// (GET /v1/interventions/{id}/desktop, Upgrade: fuse-vnc/1). it is the
// computer stream with a recording on it: an input-action log plus a
// screenshot every few seconds, saved sealed and linked from a
// takeover_session audit row. the plain computer stream answers 423 while
// the takeover is held, so this is the only way in. only the holder may
// open it.
func (h *Handler) takeoverDesktop(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeTakeover) {
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), orchestrator.VNCProto) {
		writeError(w, http.StatusBadRequest, CodeInvalidArgument,
			"takeover desktop requires Upgrade: "+orchestrator.VNCProto, nil)
		return
	}
	id := chi.URLParam(r, "interventionId")
	if !h.interventionVisible(w, r, id) {
		return
	}
	p := principal(r)
	guest, err := h.Fleet.TakeoverDesktop(r.Context(), id, p.Actor, p.Master, r.RemoteAddr)
	if err != nil {
		writeHitlError(w, err)
		return
	}
	defer func() { _ = guest.Close() }()
	serveVNC(w, guest)
}

// getRecording returns one recorded takeover session, decrypted with the
// orchestrator key (GET /v1/hitl/recordings/{id}): a terminal session as an
// asciinema v2 cast, a desktop session as json (an input-action log plus
// screenshots). takeover_session audit rows link it. needs hitl:read and a
// key covering the environment.
func (h *Handler) getRecording(w http.ResponseWriter, r *http.Request) {
	if !h.requireScope(w, r, hitl.ScopeRead) {
		return
	}
	rec, err := h.Fleet.GetRecording(r.Context(), chi.URLParam(r, "recordingId"))
	if err != nil {
		writeFleetError(w, err)
		return
	}
	if !h.covers(principal(r), rec.EnvID) {
		writeError(w, http.StatusNotFound, CodeNotFound, "recording not found", nil)
		return
	}
	if rec.Kind == "desktop" {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/x-asciicast")
	}
	w.Header().Set("Fuse-Recording-Kind", rec.Kind)
	w.Header().Set("Fuse-Intervention-Id", rec.InterventionID)
	if rec.Truncated {
		w.Header().Set("Fuse-Recording-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rec.Cast)
}
