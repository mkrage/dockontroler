package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mkrage/dockontroler/internal/manager"
)

// The two views the list can be rendered in, and the cookie that remembers which.
//
// Both are rendered on the server: the refresh already swaps in markup from the
// templates, and a view assembled in the browser would be a second place a row can be
// wrong. Which one is a cookie rather than something kept in the browser alone,
// because the server has to know before it writes the first row.
const (
	viewCards  = "cards"
	viewPanel  = "panel"
	viewCookie = "dkview"
)

// viewFromRequest decides which view to render: what the URL asks for, else what the
// cookie remembers, else the cards.
//
// An unknown value is not an error. It can only come from a hand-typed URL or a stale
// cookie, and the honest answer to both is the default view rather than a page saying
// no.
func viewFromRequest(r *http.Request) string {
	if asked := r.URL.Query().Get("view"); asked != "" {
		return knownView(asked)
	}
	if cookie, err := r.Cookie(viewCookie); err == nil {
		return knownView(cookie.Value)
	}
	return viewCards
}

func knownView(view string) string {
	if view == viewPanel {
		return viewPanel
	}
	return viewCards
}

// rememberView keeps the choice for the next page load. Only set when the request
// named a view, so a plain reload never rewrites the cookie it was just read from.
//
// HttpOnly because nothing in the browser needs to read it: app.js asks the fragment
// endpoint for a view by query parameter, and the answer to that request carries the
// cookie back.
func rememberView(w http.ResponseWriter, r *http.Request, view string) {
	if r.URL.Query().Get("view") == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     viewCookie,
		Value:    view,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// pageData is the model for the full page template.
type pageData struct {
	Overview      overviewData
	RefreshMillis int64
	AssetVersion  string
	// SelfProtected is false when docKontroler could not identify its own
	// container, meaning it cannot stop the UI from shutting itself down. Worth a
	// visible banner rather than a silent gap.
	SelfProtected bool
	Flash         *flashMessage
	// View is the list view this page was rendered in, which is what the switch in
	// the top bar marks as current.
	View string
}

// IsPanel is the question both the layout and the list template ask of the view, and
// it is a method rather than a string comparison in the templates so the two view
// names live in exactly one place.
func (p pageData) IsPanel() bool { return p.View == viewPanel }

type flashMessage struct {
	Level string // "ok" or "error"
	Text  string
}

// overviewData is the container list plus the one thing only the request knows:
// the address the browser used to get here.
//
// That address is what turns a published port into a link you can click. The
// daemon reports a binding on 0.0.0.0, and nothing on this side knows which of
// the host's addresses reaches it — the browser, by having asked, just showed one.
type overviewData struct {
	manager.Overview
	Host string
	// View decides which of the two arrangements the list template renders.
	View string
}

func (o overviewData) IsPanel() bool { return o.View == viewPanel }

// card is what the template renders one container from: the container itself, the
// host that every published port needs to become a link, and the colour of the
// stack it belongs to.
type card struct {
	manager.Container
	Host string
	// ProjectClass is the stylesheet's colour slot for this container's Compose
	// project, or "" for a container that is not part of a multi-container stack.
	ProjectClass string
}

// SearchText is the haystack the filter box matches against: every word about this
// container somebody might plausibly type to find it.
//
// Assembled here rather than scraped from the rendered card in the browser, which
// would match the words on its own buttons — "stop" would find every container on the
// host. Lowercased here too, so the browser is not doing it for thirty cards on every
// keystroke.
func (c card) SearchText() string {
	parts := []string{c.Name, c.ComposeProject, c.ComposeService, c.Image}
	for _, port := range c.Ports {
		// "which container was on 8080 again" is a question this answers.
		parts = append(parts, port.Label())
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// ShowProject decides whether the sub line names the Compose project. It is left
// out when the name would only repeat the service name above it — except on a card
// that carries a project colour, where the name is what explains the colour.
func (c card) ShowProject() bool {
	if c.ComposeProject == "" {
		return false
	}
	return c.ProjectClass != "" || c.ComposeProject != c.ComposeService
}

// stack is one Compose project in the strip above the grid: what it is called, how
// much of it is up, and the colour its cards carry.
type stack struct {
	Project string
	Class   string
	Active  int
	Total   int
	// HasSelf is true when docKontroler runs in this project, which is why stopping
	// it will leave one container behind.
	HasSelf bool
}

// unit is one thing the panel view can start, restart and stop as a whole: a Compose
// project, whatever its size, or a container that belongs to no project.
//
// The card view has no such notion, and for good reasons — a stack is a chip there, a
// lone container is its own card. But a control panel needs every row to be the same
// kind of thing, or the two sorts the strip leaves out get no switch anywhere: a
// project holding a single container, and a container Compose never touched.
type unit struct {
	// Name is what the row is called: the Compose project, or the container's own
	// name for a unit that is one container.
	Name string

	// Project is the Compose project this row acts on, empty for a lone container.
	// It is also what decides where the buttons post; see URL.
	Project string

	// Class is the stylesheet's colour slot for the project, matching the cards in
	// the other view. Empty for anything that has no colour there either.
	Class string

	// Containers are the members, in the order the overview sorted them.
	Containers []card

	// Host is the address the browser used, which is what turns a port into a link.
	Host string
}

// Total and ActiveCount are the fraction the row shows: how much of this unit is up.
func (u unit) Total() int { return len(u.Containers) }

func (u unit) ActiveCount() int {
	active := 0
	for _, container := range u.Containers {
		if container.Active() {
			active++
		}
	}
	return active
}

// Multi reports whether this unit is more than one container, which is what decides
// whether its buttons say "all" — and whether stopping it is worth a confirmation.
func (u unit) Multi() bool { return len(u.Containers) > 1 }

// AllActive and AnyActive decide which of the three buttons can do anything.
func (u unit) AllActive() bool { return u.Total() > 0 && u.ActiveCount() == u.Total() }

func (u unit) AnyActive() bool { return u.ActiveCount() > 0 }

// SelfOnly marks the row that is docKontroler and nothing else. Its Stop and Restart
// would be refused, so they are offered as dead buttons saying why instead — the same
// answer its card gives in the other view.
func (u unit) SelfOnly() bool { return u.Total() == 1 && u.HasSelf() }

// HasSelf reports whether docKontroler itself is in this unit, which is why stopping
// it will leave one container behind.
func (u unit) HasSelf() bool {
	for _, container := range u.Containers {
		if container.IsSelf {
			return true
		}
	}
	return false
}

// StateClass is the one colour the row carries on its edge.
//
// It is the worst state in the unit, because a column of thirty rows is scanned for
// what is wrong, not for what is fine. A unit that is only partly up is deliberately
// *not* one of those: half a stack running is as often somebody's intention — a
// backup container in the project, a service scaled to zero — as it is a fault, and a
// row that sits amber forever teaches you to ignore amber. The fraction beside the
// bar says it instead.
func (u unit) StateClass() string {
	active, transitional := 0, 0
	for _, container := range u.Containers {
		switch container.StateClass() {
		case "error":
			return "error"
		case "transitional":
			transitional++
		}
		if container.Active() {
			active++
		}
	}
	switch {
	case transitional > 0:
		return "transitional"
	case active > 0:
		return "running"
	default:
		return "stopped"
	}
}

// URL is where a button on this row posts.
//
// A project posts to the stack routes whatever its size: those act on the Compose
// label, so a project of one container is not a special case there — and going
// through them means it is stopped in dependency order and reported as a stack, like
// every other project. A container with no project has no stack to post to, so its
// row drives the container routes directly.
func (u unit) URL(action string) string {
	if u.Project != "" {
		return "/stacks/" + url.PathEscape(u.Project) + "/" + action
	}
	if len(u.Containers) == 0 {
		return ""
	}
	return "/containers/" + url.PathEscape(u.Containers[0].ID) + "/" + action
}

// StopConfirm and RestartConfirm are what the browser asks before the two buttons
// that interrupt a service, or "" for a row where no question is warranted.
//
// Only a unit of more than one container is asked about, which is the rule the strip
// in the other view follows: one container is one card's worth of consequence, and a
// dialog in front of every stop would train the reflex that dismisses it.
func (u unit) StopConfirm() string {
	if !u.Multi() {
		return ""
	}
	text := fmt.Sprintf("Stop the whole %s stack?\n\n%d of %d containers are running and will be stopped, the ones that depend on others first.",
		u.Name, u.ActiveCount(), u.Total())
	if u.HasSelf() {
		text += "\n\ndocKontroler itself belongs to this stack and will be left running."
	}
	return text
}

func (u unit) RestartConfirm() string {
	if !u.Multi() {
		return ""
	}
	return fmt.Sprintf("Restart the whole %s stack?\n\nThe %d running containers go down and come back one at a time, dependencies first. Whatever is stopped stays stopped.",
		u.Name, u.ActiveCount())
}

// SearchText is what the filter box matches the row *itself* against: its name, and
// nothing its containers contribute.
//
// That is the whole point of the split. Each container carries its own haystack, and
// the two answer different questions: whether the row is shown at all — it is, if
// either it or any of its containers match — and whether the containers inside it are
// filtered too. A row whose own name you typed keeps all of them, because you were
// looking for the stack; a row found through one of its services shows that service.
// Fold the members' words in here and every row found by a service would open onto
// everything standing beside it.
func (u unit) SearchText() string { return strings.ToLower(u.Name) }

// unitPort is one way into a unit: a published port of one of its containers, and
// whether anything is listening on it right now.
type unitPort struct {
	manager.PortMapping
	Host string
	// Live is true when the container publishing this port is running, which is what
	// decides whether the port is offered as a link.
	Live bool
	// Container is which member publishes it — the part the row cannot show, since
	// the port is listed for the unit as a whole.
	Container string
}

// Link is where to send the browser, or "" when there is nothing useful to open:
// a udp port, or a mapping whose container is not running.
func (p unitPort) Link() string {
	if !p.Live {
		return ""
	}
	return p.URL(p.Host)
}

func (p unitPort) Title() string { return p.Container + ": " + p.Detail() }

// Ports are the ways into this unit, gathered from its containers and deduplicated.
//
// One list for the whole row rather than one per container, because "which port was
// that again" is a question about the service, not about which container of it
// happens to publish the port. The members carry their own again once the row is
// folded open, which is where that distinction starts to matter.
func (u unit) Ports() []unitPort {
	var ports []unitPort
	at := map[manager.PortMapping]int{}
	for _, container := range u.Containers {
		for _, mapping := range container.Ports {
			if index, seen := at[mapping]; seen {
				// Two containers publishing the same mapping cannot both be up, but one
				// of them being up is what decides whether this is a link.
				if container.Running && !ports[index].Live {
					ports[index].Live = true
					ports[index].Container = container.Name
				}
				continue
			}
			at[mapping] = len(ports)
			ports = append(ports, unitPort{
				PortMapping: mapping,
				Host:        u.Host,
				Live:        container.Running,
				Container:   container.Name,
			})
		}
	}
	// By port number across the whole unit: the members are sorted by name, so
	// without this the numbers run backwards wherever the alphabet does.
	sort.SliceStable(ports, func(i, j int) bool { return ports[i].Port() < ports[j].Port() })
	return ports
}

// projectColours is how many colour slots app.css defines — and it has to stay in
// step with it, which is what TestProjectColoursMatchTheStylesheet is for.
//
// Ten, because the slots wrap and every wrap puts two stacks in the same colour: with
// six, a host running seven stacks gave its first and its last one, side by side in the
// same strip. Ten is where the palette stops — it is how many colour families can be
// told apart once running green, transitional amber, dead red and stopped grey are
// spoken for, and the reasoning behind each one is in app.css. An eleventh stack starts
// over at pc0.
const projectColours = 10

// Active and Stopped split the overview into the grid you look at and the section
// below it.
//
// The split lives here, not in the manager: Overview.Groups is what /api/containers
// serves, and a scripted consumer should keep getting every container in one
// predictable shape no matter how the page happens to arrange them today.
func (o overviewData) Active() []card { return o.cards(true) }

func (o overviewData) Stopped() []card { return o.cards(false) }

// Stacks is the strip above the grid: every Compose project that has more than one
// container, in the same order — and the same colour — as its cards.
func (o overviewData) Stacks() []stack {
	classes := projectClasses(o.Groups)

	var stacks []stack
	for _, group := range o.Groups {
		if !group.Stack() {
			continue
		}
		stacks = append(stacks, stack{
			Project: group.Project,
			Class:   classes[group.Project],
			Active:  group.ActiveCount(),
			Total:   len(group.Containers),
			HasSelf: group.HasSelf(),
		})
	}
	return stacks
}

// Units is the panel view: one row per thing that can be switched as a whole, in the
// order the overview sorted the projects, and the containers without one last.
func (o overviewData) Units() []unit {
	classes := projectClasses(o.Groups)

	var units []unit
	for _, group := range o.Groups {
		if group.Project == "" {
			// One row each, not one row called "no project": these containers share
			// nothing but the absence of one, and a Start all above them would be a
			// button that starts unrelated things together.
			for _, container := range group.Containers {
				units = append(units, unit{
					Name:       container.Name,
					Host:       o.Host,
					Containers: []card{{Container: container, Host: o.Host}},
				})
			}
			continue
		}

		members := make([]card, 0, len(group.Containers))
		for _, container := range group.Containers {
			members = append(members, card{
				Container:    container,
				Host:         o.Host,
				ProjectClass: classes[group.Project],
			})
		}
		units = append(units, unit{
			Name:       group.Project,
			Project:    group.Project,
			Class:      classes[group.Project],
			Host:       o.Host,
			Containers: members,
		})
	}
	return units
}

// cards flattens the groups, keeping their order, so containers of one Compose
// project stay next to each other in the grid.
func (o overviewData) cards(active bool) []card {
	classes := projectClasses(o.Groups)

	var cards []card
	for _, group := range o.Groups {
		for _, container := range group.Containers {
			if container.Active() == active {
				cards = append(cards, card{
					Container:    container,
					Host:         o.Host,
					ProjectClass: classes[container.ComposeProject],
				})
			}
		}
	}
	return cards
}

// projectClasses hands every multi-container project one of the stylesheet's colour
// slots. It is what replaces the per-project headings the list used to be cut into:
// the cards of a stack are already adjacent, and the colour is what makes that
// visible in a grid.
//
// Slots go out in the order the projects are listed, not hashed from the name.
// Adjacency is the whole point, so what matters is that neighbours differ — and
// consecutive slots are the ones the palette keeps furthest apart. It also means the
// same project keeps its colour across the grid and the not-running section below
// it, and across a refresh, since the order is stable.
func projectClasses(groups []manager.Group) map[string]string {
	classes := map[string]string{}
	for _, group := range groups {
		if group.Stack() {
			classes[group.Project] = fmt.Sprintf("pc%d", len(classes)%projectColours)
		}
	}
	return classes
}

// linkHost is the request's host without its port, ready to drop into a URL.
// IPv6 literals keep their brackets, or the link would end at the first colon.
func linkHost(r *http.Request) string {
	host := r.Host
	if stripped, _, err := net.SplitHostPort(host); err == nil {
		host = stripped
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	overview, err := s.manager.List(r.Context())
	if err != nil {
		s.log.Error("could not list containers", "error", err)
		http.Error(w, "Cannot reach the Docker daemon: "+manager.UserMessage(err), http.StatusBadGateway)
		return
	}

	view := viewFromRequest(r)
	data := pageData{
		Overview:      overviewData{Overview: overview, Host: linkHost(r), View: view},
		RefreshMillis: s.refreshInterval.Milliseconds(),
		AssetVersion:  s.assetVersion,
		SelfProtected: s.manager.SelfID() != "",
		Flash:         flashFromQuery(r.URL.Query()),
		View:          view,
	}

	rememberView(w, r, view)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The overview reflects live daemon state, so it must never be cached.
	w.Header().Set("Cache-Control", "no-store")
	if err := s.templates.ExecuteTemplate(w, "page", data); err != nil {
		// Too late for a status code — the response is already streaming.
		s.log.Error("could not render the page", "error", err)
	}
}

// handleFragment serves just the container list, which is what the auto-refresh
// fetches. Rendering it from the same template as the full page keeps the markup
// in one place instead of rebuilding rows in JavaScript.
func (s *Server) handleFragment(w http.ResponseWriter, r *http.Request) {
	overview, err := s.manager.List(r.Context())
	if err != nil {
		s.log.Error("could not list containers", "error", err)
		http.Error(w, manager.UserMessage(err), http.StatusBadGateway)
		return
	}

	view := viewFromRequest(r)
	// Asking for a view here is how the switch in the top bar works without a page
	// load, so this is also where that choice has to be recorded for the next one.
	rememberView(w, r, view)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := overviewData{Overview: overview, Host: linkHost(r), View: view}
	if err := s.templates.ExecuteTemplate(w, "containers", data); err != nil {
		s.log.Error("could not render the container list", "error", err)
	}
}

// handleAPIContainers exposes the overview as JSON. Nothing in the UI needs it —
// it is there so users can script against docKontroler.
func (s *Server) handleAPIContainers(w http.ResponseWriter, r *http.Request) {
	overview, err := s.manager.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, actionResponse{Message: manager.UserMessage(err)})
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// handleAction builds the handler for start, stop, restart and recreate.
func (s *Server) handleAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		var (
			message string
			err     error
		)
		switch action {
		case "start":
			err = s.manager.Start(r.Context(), id)
			message = "Container started."
		case "stop":
			err = s.manager.Stop(r.Context(), id)
			message = "Container stopped."
		case "restart":
			err = s.manager.Restart(r.Context(), id)
			message = "Container restarted."
		case "recreate":
			var result manager.RecreateResult
			result, err = s.manager.Recreate(r.Context(), id)
			message = joinMessage("Container recreated.", result.Notes)
		default:
			// Unreachable: actions come from the route table, not from input.
			http.Error(w, "unknown action", http.StatusNotFound)
			return
		}

		s.finishAction(w, r, action, id, message, err)
	}
}

// handleStack builds the handler for starting, stopping or restarting a whole
// Compose project.
//
// It reports partial outcomes rather than only success or failure: a stack where
// one container refused to stop is neither, and "3 stopped, 1 failed" is the only
// answer that tells the user what to do next.
func (s *Server) handleStack(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")

		var (
			result manager.StackResult
			err    error
		)
		switch action {
		case "stop":
			result, err = s.manager.StopStack(r.Context(), project)
		case "restart":
			result, err = s.manager.RestartStack(r.Context(), project)
		default:
			result, err = s.manager.StartStack(r.Context(), project)
		}

		s.finishAction(w, r, "stack "+action, project,
			joinMessage(result.Message(), result.Notes), err)
	}
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := r.ParseForm(); err != nil {
		s.finishAction(w, r, "policy", id, "", fmt.Errorf("%w: malformed form data", manager.ErrUnsupported))
		return
	}
	policy := r.PostFormValue("policy")

	err := s.manager.SetPolicy(r.Context(), id, policy)
	s.finishAction(w, r, "policy", id, "Restart policy set to "+policy+".", err)
}

// finishAction reports the outcome of an action in whichever form the caller
// expects, and logs failures.
func (s *Server) finishAction(w http.ResponseWriter, r *http.Request, action, id, successMessage string, err error) {
	if err != nil {
		message := manager.UserMessage(err)
		status := statusFor(err)
		// Refusals and races are expected traffic; only genuine faults are
		// logged at error level.
		if status >= http.StatusInternalServerError {
			s.log.Error("action failed", "action", action, "container", id, "error", err)
		} else {
			s.log.Info("action refused", "action", action, "container", id, "reason", message)
		}
		s.respond(w, r, status, "error", message)
		return
	}
	s.respond(w, r, http.StatusOK, "ok", successMessage)
}

// respond answers an action request.
//
// A fetch from the page sends Accept: application/json and gets JSON back, so the
// list can be refreshed in place. A plain form submission — the path taken when
// JavaScript is unavailable — gets a redirect to the overview with the message in
// the query string.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, level, message string) {
	if wantsJSON(r) {
		writeJSON(w, status, actionResponse{OK: level == "ok", Message: message})
		return
	}

	target := url.URL{Path: "/"}
	if message != "" {
		target.RawQuery = url.Values{"level": {level}, "msg": {message}}.Encode()
	}
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

type actionResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Nothing useful left to do; the status line is already sent.
		return
	}
}

// statusFor maps the manager's sentinel errors onto HTTP status codes.
func statusFor(err error) int {
	switch {
	case errors.Is(err, manager.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, manager.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, manager.ErrProtected):
		return http.StatusForbidden
	case errors.Is(err, manager.ErrUnsupported):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// flashFromQuery reads a message left by a redirect. The level is restricted to
// known values so a crafted link cannot style itself as a success.
func flashFromQuery(query url.Values) *flashMessage {
	text := strings.TrimSpace(query.Get("msg"))
	if text == "" {
		return nil
	}
	level := "error"
	if query.Get("level") == "ok" {
		level = "ok"
	}
	// Keep a hostile link from filling the page with text. The templates escape
	// the content, so this is about layout rather than safety.
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return &flashMessage{Level: level, Text: text}
}

func joinMessage(headline string, notes []string) string {
	if len(notes) == 0 {
		return headline
	}
	return headline + " " + strings.Join(notes, "; ") + "."
}
