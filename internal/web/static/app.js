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
	var refreshMillis = Number(document.body.dataset.refreshMillis) || 0;

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

	// null when there is no such section, which is not the same as a folded one: a
	// section that has just appeared — the first container to stop — has to keep the
	// state the server rendered it with instead of inheriting "folded" from its own
	// absence a moment ago.
	function stoppedIsOpen() {
		var section = containers.querySelector(".stopped");
		return section ? section.open : null;
	}

	function setStoppedOpen(open) {
		var section = containers.querySelector(".stopped");
		if (section && open !== null) {
			section.open = open;
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
		var groups = containers.querySelectorAll(".stacks, .rows, .stopped");
		for (var k = 0; k < groups.length; k++) {
			groups[k].hidden = !groups[k].querySelector(".row:not([hidden]), .stack:not([hidden])");
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

		return fetch("/partials/containers", {
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
				// The refreshed markup arrives with the "not running" section in the
				// state the server renders it in, so reinserting it would undo a fold
				// the user just made. Comparing incoming markup against incoming markup
				// means this survives every later poll too.
				var wasOpen = stoppedIsOpen();
				containers.innerHTML = signature;
				setStoppedOpen(wasOpen);
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

		// The card for a container action, the chip for a stack one: whichever it is,
		// it greys out until the answer comes back. A stack stop is the slowest thing
		// here — it walks its containers one at a time — so having something to look at
		// matters more there than anywhere else.
		var busy = form.closest(".row, .stack");
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

	if (filterBox) {
		// Only now does the box exist as far as the user is concerned; see the template
		// for why it is rendered hidden.
		filterBox.hidden = false;
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
