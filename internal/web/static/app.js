/* Dockontroler front-end.
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
				containers.innerHTML = signature;
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

		var row = form.closest(".row");
		inFlight++;
		if (row) {
			row.classList.add("is-busy");
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
			showToast("error", "Could not reach Dockontroler: " + error.message);
		}).then(function () {
			inFlight--;
			if (row) {
				row.classList.remove("is-busy");
			}
			// Force it: the whole point of the action was to change something.
			return refresh(true);
		});
	});

	if (refreshButton) {
		refreshButton.addEventListener("click", function () {
			refresh(true);
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

	if (refreshMillis >= 1000) {
		window.setInterval(function () {
			refresh(false);
		}, refreshMillis);
	}
})();
