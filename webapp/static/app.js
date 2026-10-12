"use strict";

// Streams of replies being written, closed when the page is swapped away.
const streams = new Set();

const $ = (selector, root = document) => root.querySelector(selector);

function setStatus(text) {
  const status = $("#status");
  if (status) status.textContent = text || "";
}

function setBusy(turnId) {
  const composer = $("#composer");
  if (!composer) return;
  if (turnId) composer.dataset.turn = turnId;
  else delete composer.dataset.turn;
  $(".send", composer).hidden = !!turnId;
  $(".stop", composer).hidden = !turnId;
}

// keepBottom runs change and keeps the view pinned to the newest message,
// unless the reader had scrolled up.
function keepBottom(change, force) {
  const box = $("#messages");
  if (!box) return change();
  const atBottom = force || box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  change();
  if (atBottom) box.scrollTop = box.scrollHeight;
}

function stream(turn) {
  if (turn.dataset.started) return;
  turn.dataset.started = "1";
  setBusy(turn.dataset.turn);

  const source = new EventSource(turn.dataset.stream);
  streams.add(source);
  const close = () => {
    source.close();
    streams.delete(source);
    setBusy(null);
  };

  // A thinking panel the reader opened or closed stays that way while the
  // reply is redrawn.
  turn.addEventListener("toggle", (e) => {
    if (e.target.matches(".thinking")) turn.dataset.thinking = e.target.open ? "open" : "closed";
  }, true);
  const replace = (target, html) => {
    keepBottom(() => {
      target.innerHTML = html;
      const thinking = $(".thinking", target);
      if (thinking && turn.dataset.thinking) thinking.open = turn.dataset.thinking === "open";
      const thought = $(".thought", target);
      if (thought) thought.scrollTop = thought.scrollHeight;
    });
  };

  source.addEventListener("update", (e) => replace(turn, e.data));
  source.addEventListener("done", (e) => {
    const box = document.createElement("div");
    box.innerHTML = e.data;
    const final = box.firstElementChild;
    replace(turn, final ? final.innerHTML : "");
    delete turn.dataset.stream;
    setBusy(null);
  });
  source.addEventListener("title", (e) => {
    const title = $("#chat-title");
    if (title) title.textContent = e.data;
    document.title = e.data + " · Souvenir";
  });
  source.addEventListener("sidebar", (e) => {
    const nav = $("#conversations");
    if (!nav) return;
    nav.innerHTML = e.data;
    htmx.process(nav);
  });
  source.addEventListener("end", close);
  source.addEventListener("error", () => {
    // The browser retries a dropped stream by itself; a closed one is over.
    if (source.readyState === EventSource.CLOSED) close();
  });
}

document.addEventListener("htmx:load", (e) => {
  e.detail.elt.querySelectorAll?.("[data-stream]").forEach(stream);
  if (e.detail.elt.matches?.("[data-stream]")) stream(e.detail.elt);
});

document.addEventListener("DOMContentLoaded", () => {
  const box = $("#messages");
  if (box) box.scrollTop = box.scrollHeight;
});

// Leaving the page for another conversation stops listening to this one.
document.addEventListener("htmx:beforeSwap", (e) => {
  if (e.detail.target !== document.body) return;
  streams.forEach((s) => s.close());
  streams.clear();
});

document.addEventListener("htmx:beforeRequest", (e) => {
  const composer = $("#composer");
  if (e.detail.elt !== composer) return;
  if (composer.dataset.turn) {
    e.preventDefault();
    setStatus("Wait for the reply to finish, or stop it.");
    return;
  }
  setStatus("");
  $(".send", composer).disabled = true;
});

document.addEventListener("htmx:afterRequest", (e) => {
  const composer = $("#composer");
  if (e.detail.elt !== composer) return;
  $(".send", composer).disabled = false;
  if (!e.detail.successful) return;
  const input = $("textarea", composer);
  input.value = "";
  resize(input);
  keepBottom(() => {}, true);
});

document.addEventListener("htmx:responseError", (e) => {
  setStatus(e.detail.xhr.responseText || "Something went wrong.");
});

document.addEventListener("htmx:sendError", () => {
  setStatus("Could not reach the server.");
});

function resize(input) {
  input.style.height = "auto";
  input.style.height = Math.min(input.scrollHeight, 200) + "px";
}

document.addEventListener("input", (e) => {
  if (e.target.matches("#composer textarea")) resize(e.target);
});

// Enter sends, Shift+Enter starts a new line.
document.addEventListener("keydown", (e) => {
  if (!e.target.matches("#composer textarea")) return;
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    if (e.target.value.trim()) e.target.form.requestSubmit();
  }
});

document.addEventListener("click", async (e) => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    const message = copy.closest("[data-raw]");
    try {
      await navigator.clipboard.writeText(message.dataset.raw);
      copy.classList.add("copied");
      setTimeout(() => copy.classList.remove("copied"), 1200);
    } catch {
      setStatus("Could not copy to the clipboard.");
    }
    return;
  }

  const stop = e.target.closest(".stop");
  if (stop) {
    const turn = $("#composer").dataset.turn;
    if (!turn) return;
    const response = await fetch("/turns/" + encodeURIComponent(turn) + "/cancel", { method: "POST" });
    if (!response.ok && response.status !== 404) setStatus("Could not stop the reply.");
    return;
  }

  // Picking a conversation on a phone closes the drawer.
  if (e.target.closest(".conversations a, .new-chat")) {
    const drawer = $("#drawer");
    if (drawer) drawer.checked = false;
  }
});
