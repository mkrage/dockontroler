/* docKontroler front-end.
 *
 * The page works without this file: every button is a real form that posts and
 * redirects. All this adds is doing it without a full page load, plus the
 * periodic refresh.
 *
 * No build step, no framework — the markup comes from the Go templates, and this
 * only ever swaps in server-rendered HTML. There is deliberately no client-side
 * rendering, so there is only one place row markup can be wrong.
 */
(function () {
	"use strict";

	var containers = document.getElementById("containers");
	var toast = document.getElementById("toast");
	var pulse = document.getElementById("pulse");
	var stamp = document.getElementById("stamp");
	var refreshButton = document.getElementById("refresh-now");
	var filterBox = document.getElementById("filter");
	var viewSwitch = document.querySelector(".viewswitch");
	var refreshMillis = Number(document.body.dataset.refreshMillis) || 0;
	// Which arrangement the list is in. The server rendered it and renders every
	// refresh, so this is only ever passed back to it — nothing here lays out a row.
	var view = document.body.dataset.view === "panel" ? "panel" : "cards";

	if (!containers) {
		return;
	}

	// Number of actions currently running. The refresh pauses while any are, so
	// the list cannot be swapped out from under a click.
	var inFlight = 0;
	// Markup of the last render, with the timestamp stripped out. Comparing
	// against it means an unchanged list is not re-inserted at all, which keeps
	// hover states, text selection and mid-click buttons intact.
	var lastSignature = null;
	var toastTimer = null;

	function showToast(level, text) {
		if (!toast) {
			return;
		}
		toast.textContent = text;
		toast.className = "toast toast--" + level;
		toast.hidden = false;

		window.clearTimeout(toastTimer);
		// Errors deserve longer than confirmations.
		toastTimer = window.setTimeout(function () {
			toast.hidden = true;
		}, level === "error" ? 9000 : 3500);
	}

	function blinkPulse() {
		if (!pulse) {
			return;
		}
		pulse.classList.add("is-active");
		window.setTimeout(function () {
			pulse.classList.remove("is-active");
		}, 300);
	}

	// What is folded right now, keyed by the name in data-fold: the two sections of the
	// card view, and every row of the panel.
	//
	// Something that is not on the page at all is left out rather than recorded as
	// folded: a section or a row that has just appeared — the first container to stop, a
	// stack somebody deployed a minute ago — has to keep the state the server rendered
	// it with instead of inheriting "folded" from its own absence a moment ago.
	function foldState() {
		var state = {};
		var folds = containers.querySelectorAll("[data-fold]");
		for (var i = 0; i < folds.length; i++) {
			state[folds[i].dataset.fold] = folds[i].open;
		}
		return state;
	}

	function restoreFolds(state) {
		var folds = containers.querySelectorAll("[data-fold]");
		for (var i = 0; i < folds.length; i++) {
			var was = state[folds[i].dataset.fold];
			if (was !== undefined) {
				folds[i].open = was;
			}
		}
	}

	// Hides everything that does not match what is in the filter box.
	//
	// It works on the rendered list rather than asking the server for a filtered one:
	// the list is already here, and a box that answers between keystrokes is the whole
	// point. Which also means it has to run again after every refresh, since the markup
	// it hides is thrown away and replaced.
	//
	// Words are matched independently, so "blog db" finds the database of the blog
	// stack without depending on the order the card happens to name them in.
	function applyFilter() {
		var query = filterBox ? filterBox.value.trim().toLowerCase() : "";
		var terms = query.split(/\s+/).filter(Boolean);

		function matches(haystack) {
			return terms.every(function (term) {
				return haystack.indexOf(term) >= 0;
			});
		}

		// Which projects still have a card on screen. An empty query leaves terms empty,
		// every() is then vacuously true, and nothing is hidden at all.
		var projectsShown = {};
		var shown = 0;
		var cards = containers.querySelectorAll(".row");
		for (var i = 0; i < cards.length; i++) {
			var card = cards[i];
			var hit = matches(card.dataset.search || "");
			card.hidden = !hit;
			if (hit) {
				shown++;
				if (card.dataset.project) {
					projectsShown[card.dataset.project] = true;
				}
			}
		}

		// The panel view. A row is shown if its own name matches or any of its containers
		// do, and the containers inside it are filtered as well — so unfolding a row you
		// searched for shows what you were looking for rather than everything standing
		// next to it. A row whose own name matched keeps all of them: you were looking for
		// the stack, not for one service in it. The row's data-search is deliberately its
		// name alone; that is what makes the two cases distinguishable.
		var units = containers.querySelectorAll(".unit");
		for (var u = 0; u < units.length; u++) {
			var ownHit = matches(units[u].dataset.search || "");
			var members = units[u].querySelectorAll(".member");
			var membersShown = 0;
			for (var m = 0; m < members.length; m++) {
				var memberHit = ownHit || matches(members[m].dataset.search || "");
				members[m].hidden = !memberHit;
				if (memberHit) {
					membersShown++;
				}
			}
			units[u].hidden = !(ownHit || membersShown > 0);
			if (!units[u].hidden) {
				shown++;
			}
		}

		// A chip stays as long as one of its containers is still shown, or its own name
		// matches: the point of finding a service is often to act on the stack around it,
		// and the control for that must not vanish just as you found it.
		var chips = containers.querySelectorAll(".stack");
		for (var j = 0; j < chips.length; j++) {
			var project = chips[j].dataset.project || "";
			chips[j].hidden = terms.length > 0 &&
				!projectsShown[project] && !matches(project.toLowerCase());
		}

		// A list, a section or the strip with nothing left in it goes as well, or the
		// page keeps their headings and gaps around nothing.
		var groups = containers.querySelectorAll(".stacks, .rows, .fold, .panel");
		for (var k = 0; k < groups.length; k++) {
			groups[k].hidden = !groups[k].querySelector(
				".row:not([hidden]), .stack:not([hidden]), .unit:not([hidden])");
		}

		var nothing = containers.querySelector("#no-matches");
		if (nothing) {
			nothing.hidden = !(terms.length > 0 && shown === 0);
			if (!nothing.hidden) {
				nothing.textContent = "No container matches “" + query + "”.";
			}
		}
	}

	function refresh(force) {
		if (!force && (inFlight > 0 || document.hidden)) {
			return Promise.resolve();
		}

		// The view goes with the request: the server renders both, and asking for one
		// here is also what records the choice for the next full page load.
		return fetch("/partials/containers?view=" + encodeURIComponent(view), {
			headers: { Accept: "text/html" },
			cache: "no-store"
		}).then(function (response) {
			if (!response.ok) {
				throw new Error("HTTP " + response.status);
			}
			return response.text();
		}).then(function (html) {
			var incoming = document.createElement("div");
			incoming.innerHTML = html;

			// The "updated 14:03:22" line changes every second and would defeat
			// the comparison below, so it is pulled out and shown in the header
			// instead.
			var incomingStamp = incoming.querySelector(".overview__stamp");
			if (incomingStamp) {
				if (stamp) {
					stamp.textContent = incomingStamp.textContent.trim();
				}
				incomingStamp.remove();
			}

			var signature = incoming.innerHTML;
			if (signature !== lastSignature) {
				// The refreshed markup arrives with every section and row in the state
				// the server renders it in, so reinserting it would undo a fold the user
				// just made. Comparing incoming markup against incoming markup means
				// this survives every later poll too.
				var folds = foldState();
				containers.innerHTML = signature;
				restoreFolds(folds);
				// The markup arrives unfiltered — the server knows nothing about the box.
				applyFilter();
				lastSignature = signature;
			}
			blinkPulse();
		}).catch(function () {
			// A missed poll is not worth telling the user about; the next one
			// will either work or the page will visibly stop updating.
		});
	}

	// One delegated listener, because the rows are replaced wholesale on every
	// refresh and per-form listeners would not survive that.
	document.addEventListener("submit", function (event) {
		var form = event.target;
		if (!(form instanceof HTMLFormElement) || !containers.contains(form)) {
			return;
		}
		event.preventDefault();

		if (form.dataset.confirm && !window.confirm(form.dataset.confirm)) {
			return;
		}

		var body = new FormData(form);
		// FormData never includes the button that submitted the form, but the
		// restart-policy control relies on exactly that: three submit buttons
		// sharing one form, each carrying its own value.
		var submitter = event.submitter;
		if (submitter && submitter.name) {
			body.append(submitter.name, submitter.value);
		}

		// Whatever the form sits in greys out until the answer comes back: the card or
		// the chip in one view, the row or one container inside it in the other. A stack
		// stop is the slowest thing here — it walks its containers one at a time — so
		// having something to look at matters more there than anywhere else.
		// Innermost first, so a container's own button does not grey out the whole row.
		var busy = form.closest(".member, .row, .unit, .stack");
		inFlight++;
		if (busy) {
			busy.classList.add("is-busy");
		}

		fetch(form.action, {
			method: "POST",
			headers: { Accept: "application/json" },
			body: new URLSearchParams(body)
		}).then(function (response) {
			return response.json().catch(function () {
				return {};
			}).then(function (payload) {
				var fallback = response.ok ? "Done." : "Request failed (HTTP " + response.status + ").";
				showToast(response.ok ? "ok" : "error", payload.message || fallback);
			});
		}).catch(function (error) {
			showToast("error", "Could not reach docKontroler: " + error.message);
		}).then(function () {
			inFlight--;
			if (busy) {
				busy.classList.remove("is-busy");
			}
			// Force it: the whole point of the action was to change something.
			return refresh(true);
		});
	});

	// The restart policy is a select, and a select with a submit button beside it
	// would put the wall of controls straight back. So changing it posts the form —
	// which lands in the submit handler above like any other action. Without
	// JavaScript the form carries its own button; see the template.
	document.addEventListener("change", function (event) {
		var select = event.target;
		if (!(select instanceof HTMLSelectElement) || !containers.contains(select)) {
			return;
		}
		// The text beside the select is what gives the control its width, so it has to
		// say what the select now says — the server renders it correctly again on the
		// next refresh, which is too late to watch your own click take effect.
		var shown = select.parentNode && select.parentNode.querySelector(".policy__text");
		if (shown && select.selectedIndex >= 0) {
			shown.textContent = select.options[select.selectedIndex].text;
		}

		var form = select.form;
		if (form && typeof form.requestSubmit === "function") {
			form.requestSubmit();
		}
	});

	if (refreshButton) {
		refreshButton.addEventListener("click", function () {
			refresh(true);
		});
	}

	// The switch is two real links and works without any of this — following one loads
	// the page in the other view. All this does is save that page load: the fragment
	// endpoint renders either view, and asking it for one is also what remembers the
	// choice for the next time the page is opened from scratch.
	if (viewSwitch) {
		viewSwitch.addEventListener("click", function (event) {
			var link = event.target.closest("a[data-view]");
			// A modified click is somebody opening the other view in a tab of its own,
			// which is a thing a link should keep being able to do.
			if (!link || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
				return;
			}
			event.preventDefault();
			if (link.dataset.view === view) {
				return;
			}

			view = link.dataset.view;
			document.body.dataset.view = view;
			// The address bar has to agree with what is on the page: a reload of a URL
			// still naming the other view would take it away again, and ?view= beats the
			// cookie on purpose — a link to one view has to win.
			if (window.history && window.history.replaceState) {
				window.history.replaceState({}, "", link.getAttribute("href"));
			}
			var options = viewSwitch.querySelectorAll("a[data-view]");
			for (var i = 0; i < options.length; i++) {
				var current = options[i] === link;
				options[i].classList.toggle("is-current", current);
				if (current) {
					options[i].setAttribute("aria-current", "page");
				} else {
					options[i].removeAttribute("aria-current");
				}
			}
			refresh(true);
		});
	}

	if (filterBox) {
		// Only now does the box exist as far as the user is concerned; see the template
		// for why it is rendered hidden. What is unhidden is the whole box rather than
		// the field alone, or its magnifier and its key hint would sit there without it.
		var searchBox = filterBox.closest(".search") || filterBox;
		searchBox.hidden = false;
		filterBox.addEventListener("input", applyFilter);
		// A type=search field fires this on its own clear button and on Escape in some
		// browsers, and nothing at all in others — hence both this and the key below.
		filterBox.addEventListener("search", applyFilter);
		filterBox.addEventListener("keydown", function (event) {
			if (event.key === "Escape" && filterBox.value !== "") {
				// Clear before the browser's own Escape handling can, so the list comes
				// back rather than the field quietly keeping a value nobody can see.
				event.preventDefault();
				filterBox.value = "";
				applyFilter();
			}
		});

		// "/" jumps to the box, as in most tools that have one. Ignored while typing
		// somewhere else, so it cannot eat the slash in a path.
		document.addEventListener("keydown", function (event) {
			if (event.key !== "/" || event.ctrlKey || event.metaKey || event.altKey) {
				return;
			}
			var focused = document.activeElement;
			if (focused && /^(INPUT|SELECT|TEXTAREA)$/.test(focused.tagName)) {
				return;
			}
			event.preventDefault();
			filterBox.focus();
			filterBox.select();
		});
	}

	// Catch up immediately when the tab comes back, rather than waiting out the
	// interval that was skipped while it was hidden.
	document.addEventListener("visibilitychange", function () {
		if (!document.hidden) {
			refresh(true);
		}
	});

	// Prime the signature so the first poll does not needlessly replace markup
	// that is already correct.
	(function primeSignature() {
		var current = containers.cloneNode(true);
		var currentStamp = current.querySelector(".overview__stamp");
		if (currentStamp) {
			if (stamp) {
				stamp.textContent = currentStamp.textContent.trim();
			}
			currentStamp.remove();
			// And out of the page as well, not only out of the copy the signature is
			// taken from: the timestamp is in the header now, and leaving the one in the
			// list until the first poll happens to remove it showed it twice for as long
			// as the refresh interval.
			var pageStamp = containers.querySelector(".overview__stamp");
			if (pageStamp) {
				pageStamp.remove();
			}
		}
		lastSignature = current.innerHTML;
	})();

	// After priming, not before: the signature has to be the unfiltered markup the
	// server sent, or every poll would look like a change. This matters when a browser
	// restores what was typed in the box across a reload.
	applyFilter();

	if (refreshMillis >= 1000) {
		window.setInterval(function () {
			refresh(false);
		}, refreshMillis);
	}
})();
