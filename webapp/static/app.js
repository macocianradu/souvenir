"use strict";

// Streams of replies being written, closed when the page is swapped away.
const streams = new Set();

const $ = (selector, root = document) => root.querySelector(selector);

// Images pasted or dropped into the composer wait there until the message is
// sent. The limits match the server's.
const MAX_IMAGES = 4;
const MAX_IMAGE_BYTES = 10 << 20;
let attached = [];

function renderAttachments() {
  const composer = $("#composer");
  if (!composer) return;
  const strip = $(".attachments", composer);
  strip.querySelectorAll("img").forEach((img) => URL.revokeObjectURL(img.src));
  strip.replaceChildren(...attached.map((file, i) => {
    const figure = document.createElement("figure");
    figure.className = "attachment";
    const img = document.createElement("img");
    img.src = URL.createObjectURL(file);
    img.alt = "Attached image";
    const remove = document.createElement("button");
    remove.type = "button";
    remove.dataset.remove = String(i);
    remove.title = "Remove image";
    remove.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>';
    figure.append(img, remove);
    return figure;
  }));
  strip.hidden = attached.length === 0;
  const files = new DataTransfer();
  attached.forEach((file) => files.items.add(file));
  $('input[name="images"]', composer).files = files.files;
}

function attach(files) {
  const images = [...files].filter((f) => f.type.startsWith("image/"));
  for (const file of images) {
    if (attached.length >= MAX_IMAGES) {
      setStatus(`A message can carry at most ${MAX_IMAGES} images.`);
      break;
    }
    if (file.size > MAX_IMAGE_BYTES) {
      setStatus(`That image is over ${MAX_IMAGE_BYTES >> 20} MB.`);
      continue;
    }
    attached.push(file);
  }
  renderAttachments();
  return images.length > 0;
}

// Pasting an image attaches it. Clipboards holding text as well, such as a
// copy from a document, paste the text as usual.
document.addEventListener("paste", (e) => {
  if (!e.target.matches?.("#composer textarea") || !e.clipboardData) return;
  if (e.clipboardData.getData("text/plain")) return;
  if (attach(e.clipboardData.files)) e.preventDefault();
});

document.addEventListener("dragover", (e) => {
  if (e.target.closest?.("#composer") && e.dataTransfer?.types.includes("Files")) e.preventDefault();
});

document.addEventListener("drop", (e) => {
  if (!e.target.closest?.("#composer") || !e.dataTransfer) return;
  if (attach(e.dataTransfer.files)) e.preventDefault();
});

// Message times are sent in the server's zone; these labels redo them in the
// reader's own zone and language, with the server's rules: the time alone
// today, then yesterday, the weekday within a week, and the date.
const clock = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" });
const weekday = new Intl.DateTimeFormat(undefined, { weekday: "short" });
const dayMonth = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short" });
const dayMonthYear = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", year: "numeric" });
const full = new Intl.DateTimeFormat(undefined, { dateStyle: "full", timeStyle: "short" });
const yesterday = (() => {
  const word = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(-1, "day");
  return word.charAt(0).toUpperCase() + word.slice(1);
})();

function stampLabel(at, now) {
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const daysAgo = (d) => new Date(today.getFullYear(), today.getMonth(), today.getDate() - d);
  const time = clock.format(at);
  if (at >= today) return time;
  if (at >= daysAgo(1)) return yesterday + " " + time;
  if (at >= daysAgo(6)) return weekday.format(at) + " " + time;
  if (at.getFullYear() === now.getFullYear()) return dayMonth.format(at) + " " + time;
  return dayMonthYear.format(at) + " " + time;
}

function localizeStamps(root) {
  const now = new Date();
  root.querySelectorAll?.("time.stamp[datetime]").forEach((el) => {
    const at = new Date(el.getAttribute("datetime"));
    if (isNaN(at)) return;
    el.textContent = stampLabel(at, now);
    el.title = full.format(at);
  });
}

setInterval(() => localizeStamps(document), 60_000);

let statusTimer;

// setStatus shows a problem, or with ok a passing confirmation.
function setStatus(text, ok) {
  const status = $("#status");
  if (!status) return;
  clearTimeout(statusTimer);
  status.textContent = text || "";
  status.classList.toggle("ok", !!ok);
  if (ok) statusTimer = setTimeout(() => setStatus(""), 1500);
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
      localizeStamps(target);
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
    if (title) {
      title.textContent = e.data;
      title.title = e.data;
    }
    const rename = $("#chat-heading .rename-btn");
    if (rename) rename.dataset.value = e.data;
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
  localizeStamps(e.detail.elt);
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
  attached = [];
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
  attached = [];
  renderAttachments();
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
    if (e.target.value.trim() || attached.length) e.target.form.requestSubmit();
  }
});

// Editing happens in place: the pencil turns a title or a memory into a
// field, Enter or leaving the field saves, and Escape cancels. The button
// says where to post (data-edit), the field's name (data-field) and what the
// answer replaces (data-target, data-swap).
function startEdit(button) {
  const holder = button.closest("li, .heading, .memory");
  if (!holder || holder.classList.contains("editing")) return;
  const before = (button.dataset.value || "").trim();
  const form = document.createElement("form");
  form.className = "rename-form";
  form.setAttribute("hx-post", button.dataset.edit);
  form.setAttribute("hx-target", button.dataset.target);
  form.setAttribute("hx-swap", button.dataset.swap || "innerHTML");
  const input = document.createElement("input");
  input.name = button.dataset.field || "title";
  input.value = before;
  input.maxLength = Number(button.dataset.max) || 200;
  input.required = true;
  input.autocomplete = "off";
  input.setAttribute("aria-label", button.getAttribute("aria-label") || "Edit");
  // The open conversation, so a rename can update its heading too.
  const active = document.createElement("input");
  active.type = "hidden";
  active.name = "active";
  active.value = $("#conversation-id")?.value || "";
  form.append(input, active);
  holder.classList.add("editing");
  holder.append(form);
  htmx.process(form);
  input.focus();
  input.select();

  let done = false;
  const finish = () => {
    done = true;
    form.remove();
    holder.classList.remove("editing");
  };
  const save = () => {
    if (done) return;
    const value = input.value.trim();
    if (!value || value === before) return finish();
    done = true;
    form.requestSubmit();
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      e.preventDefault();
      finish();
    } else if (e.key === "Enter") {
      e.preventDefault();
      save();
    }
  });
  input.addEventListener("blur", save);
  // On success the answer replaces the field.
  form.addEventListener("htmx:afterRequest", (e) => {
    if (!e.detail.successful) finish();
  });
}

document.addEventListener("click", async (e) => {
  const edit = e.target.closest("[data-edit]");
  if (edit) {
    startEdit(edit);
    return;
  }

  const remove = e.target.closest("[data-remove]");
  if (remove) {
    attached.splice(Number(remove.dataset.remove), 1);
    renderAttachments();
    return;
  }

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

// Settings save as they change; the memory box empties once added.
document.addEventListener("htmx:afterRequest", (e) => {
  if (!e.detail.successful) return;
  if (e.detail.elt.matches?.(".toggles")) setStatus("Saved.", true);
  if (e.detail.elt.matches?.(".add-memory")) e.detail.elt.reset();
});

// Writing a calendar includes reading it.
document.addEventListener("change", (e) => {
  const box = e.target;
  if (!box.matches?.(".calendars input")) return;
  const row = box.closest("tr");
  if (box.name === "write" && box.checked) $('input[name="read"]', row).checked = true;
  if (box.name === "read" && !box.checked) $('input[name="write"]', row).checked = false;
}, true);
