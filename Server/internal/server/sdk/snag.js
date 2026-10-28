/*! Snag web SDK 1.0.0 · MIT · https://github.com/CreatureSurvive/Snag
 *
 * <script src="https://snag.example.com/sdk/snag.js"></script>
 * <script>Snag.init({ url: "https://snag.example.com", apiKey: "snag_…", app: { name: "My Site", version: "2.1" } })</script>
 *
 * A "Feedback" button opens a report form. Reports carry redacted console logs and a trail of
 * recent activity (pages, clicks, requests), and optionally a screenshot of this tab with
 * private elements blacked out. Nothing is sent until the user presses Send.
 */
(function (global) {
  "use strict";

  var VERSION = "1.0.0";

  // ---------------------------------------------------------------------------------------------
  // Redaction: the same rules as the Swift TextRedactor and the server.

  var STANDARD = [
    ["private-key", /-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----/g, 0],
    ["jwt", /\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b/g, 0],
    ["bearer", /(?:bearer|token)\s+([A-Za-z0-9\-._~+/]{12,}=*)/gi, 1],
    ["secret", /(?:pass(?:word|wd)?|secret|api[_-]?key|access[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|client[_-]?secret|session[_-]?id)["']?\s*[:=]\s*["']?([^\s"'&,;}]{3,})/gi, 1],
    ["credentials", /[a-z][a-z0-9+.-]*:\/\/([^\s/:@]+:[^\s/@]+)@/gi, 1],
    ["card", /\b\d(?:[ -]?\d){12,18}\b/g, 0, luhn],
  ];
  var STRICT = STANDARD.concat([
    ["email", /[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/gi, 0],
    ["phone", /(?<![\w.])\+?\d{1,3}[ .-]?\(?\d{2,4}\)?[ .-]?\d{3,4}[ .-]?\d{3,4}(?![\w.])/g, 0],
    ["ip", /\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b/g, 0],
  ]);

  function luhn(candidate) {
    var digits = candidate.replace(/\D/g, "");
    if (digits.length < 13 || digits.length > 19 || /^(\d)\1+$/.test(digits)) return false;
    var sum = 0;
    for (var i = 0; i < digits.length; i++) {
      var d = +digits[digits.length - 1 - i];
      if (i % 2 === 1) { d *= 2; if (d > 9) d -= 9; }
      sum += d;
    }
    return sum % 10 === 0;
  }

  function redact(text, level) {
    if (level === "off" || text == null) return text;
    var rules = level === "strict" ? STRICT : STANDARD;
    var out = String(text);
    rules.forEach(function (rule) {
      var name = rule[0], re = rule[1], group = rule[2], valid = rule[3];
      out = out.replace(re, function () {
        var match = arguments[0], value = group ? arguments[group] : match;
        if (value == null || value.indexOf("[redacted:") === 0 || (valid && !valid(value))) return match;
        var at = match.lastIndexOf(value);
        return match.slice(0, at) + "[redacted:" + name + "]" + match.slice(at + value.length);
      });
    });
    return out;
  }

  // ---------------------------------------------------------------------------------------------
  // Breadcrumbs and console capture.

  function Ring(capacity) { this.capacity = capacity; this.items = []; }
  Ring.prototype.push = function (item) {
    this.items.push(item);
    if (this.items.length > this.capacity) this.items.shift();
  };

  function looksLikeIdentifier(s) {
    if (/^\d{2,}$/.test(s)) return true;
    if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(s)) return true;
    if (/^[0-9a-f]{16,}$/i.test(s)) return true;
    if (s.length >= 20 && /\d/.test(s) && /[a-z]/i.test(s)) return true;
    return s.indexOf("@") >= 0;
  }

  /** "https://u:p@api.example.com/users/123?token=x&page=2#f" → "api.example.com/users/:id?token&page" */
  function sanitizeURL(input, base) {
    var url;
    try { url = new URL(input, base || (global.location && global.location.href)); } catch (e) { return "?"; }
    var segments = url.pathname.split("/").filter(Boolean).map(function (s) {
      var decoded = s;
      try { decoded = decodeURIComponent(s); } catch (e) {}
      return looksLikeIdentifier(decoded) ? ":id" : s;
    });
    var out = url.host + (segments.length ? "/" + segments.join("/") : "");
    var names = [];
    url.searchParams.forEach(function (_, name) { if (names.indexOf(name) < 0) names.push(name); });
    if (names.length) out += "?" + names.join("&");
    return out;
  }

  function describeElement(el) {
    var named = el.closest && el.closest("[data-snag-name]");
    if (named) return named.getAttribute("data-snag-name");
    var target = (el.closest && el.closest("button, a, [role=button], input[type=submit], input[type=button], summary, label")) || el;
    var tag = target.tagName ? target.tagName.toLowerCase() : "element";
    if (target.id) return tag + "#" + target.id;
    if (tag === "a" && target.getAttribute("href")) return "link to " + sanitizeURL(target.getAttribute("href"));
    var type = target.getAttribute && target.getAttribute("type");
    return type ? tag + "[type=" + type + "]" : tag;
  }

  function formatArg(arg) {
    if (arg instanceof Error) return arg.name + ": " + arg.message + (arg.stack ? "\n" + arg.stack : "");
    if (typeof arg === "string") return arg;
    try { return JSON.stringify(arg); } catch (e) { return String(arg); }
  }

  // ---------------------------------------------------------------------------------------------
  // The form, in a shadow root so the page's CSS can't reach it (and vice versa).

  var STYLE = [
    ":host { all: initial; }",
    "* { box-sizing: border-box; font: 15px/1.4 -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }",
    ".launch { position: fixed; z-index: 2147483000; right: 20px; bottom: 20px; border: 0; border-radius: 999px; padding: 10px 16px;",
    "  background: var(--snag-accent, #3b5bdb); color: #fff; font-weight: 600; box-shadow: 0 4px 16px rgba(0,0,0,.25); cursor: pointer; }",
    ".launch:focus-visible, button:focus-visible, input:focus-visible, textarea:focus-visible { outline: 3px solid #fab005; outline-offset: 2px; }",
    "dialog { border: 0; border-radius: 14px; padding: 0; width: min(460px, calc(100vw - 24px)); max-height: calc(100vh - 24px);",
    "  background: #fff; color: #1c1c1f; box-shadow: 0 20px 60px rgba(0,0,0,.35); }",
    "dialog::backdrop { background: rgba(0,0,0,.35); }",
    "form { display: flex; flex-direction: column; gap: 12px; padding: 18px; }",
    "h2 { margin: 0; font-size: 18px; font-weight: 700; }",
    ".kinds { display: flex; gap: 6px; } .kinds label { flex: 1; } .kinds input { position: absolute; opacity: 0; }",
    ".kinds span { display: block; text-align: center; padding: 7px; border: 1px solid #d0d0d7; border-radius: 8px; cursor: pointer; }",
    ".kinds input:checked + span { background: var(--snag-accent, #3b5bdb); border-color: transparent; color: #fff; font-weight: 600; }",
    ".kinds input:focus-visible + span { outline: 3px solid #fab005; outline-offset: 2px; }",
    "input[type=text], input[type=email], textarea { width: 100%; padding: 9px 10px; border: 1px solid #c4c4cc; border-radius: 8px; background: #fff; color: inherit; }",
    "textarea { min-height: 110px; resize: vertical; }",
    ".row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }",
    ".shot { position: relative; } .shot img { max-width: 140px; max-height: 110px; border-radius: 6px; border: 1px solid #d0d0d7; display: block; }",
    "button { border: 1px solid #c4c4cc; background: #f3f3f6; color: inherit; border-radius: 8px; padding: 8px 12px; cursor: pointer; }",
    "button.primary { background: var(--snag-accent, #3b5bdb); border-color: transparent; color: #fff; font-weight: 600; }",
    "button:disabled { opacity: .55; cursor: default; }",
    ".hint { font-size: 13px; color: #5c5c66; margin: 0; } .error { color: #c92a2a; font-size: 14px; margin: 0; }",
    ".actions { display: flex; justify-content: flex-end; gap: 8px; }",
    ".done { padding: 28px 18px; text-align: center; } .done p { margin: 0 0 16px; font-size: 17px; font-weight: 600; }",
    "@media (prefers-color-scheme: dark) {",
    "  dialog { background: #1b1b1f; color: #ededf0; } input[type=text], input[type=email], textarea { background: #111113; border-color: #3a3a42; }",
    "  button { background: #2a2a30; border-color: #3a3a42; } .hint { color: #a0a0aa; } .kinds span { border-color: #3a3a42; } }",
    "@media (prefers-reduced-motion: no-preference) { dialog[open] { animation: snag-in .15s ease-out; } }",
    "@keyframes snag-in { from { transform: translateY(8px); opacity: 0; } }",
  ].join("\n");

  var DEFAULT_TEXT = {
    button: "Feedback", title: "Send Feedback", bug: "Bug", feedback: "Feedback", feature: "Idea",
    summary: "Summary", message: "What happened? What did you expect?", email: "Email (optional)",
    emailHint: "Only used to follow up on this report.",
    screenshot: "Add a screenshot of this page", removeScreenshot: "Remove screenshot",
    screenshotHint: "Your browser asks what to share: choose this tab. Passwords and private areas are blacked out.",
    wrongSurface: "Share this tab (not a window or screen) so private areas can be hidden.",
    privacy: "Recent console messages and activity on this page are included, with passwords and tokens removed.",
    cancel: "Cancel", send: "Send", sending: "Sending…", sent: "Thanks! Your report was sent.",
    queued: "Thanks! Your report is saved and will be sent when you're back online.", close: "Close",
    failed: "Couldn't send the report: ",
  };

  // ---------------------------------------------------------------------------------------------

  function uuid() {
    if (global.crypto && global.crypto.randomUUID) return global.crypto.randomUUID();
    var b = new Uint8Array(16);
    global.crypto.getRandomValues(b);
    b[6] = (b[6] & 0x0f) | 0x40; b[8] = (b[8] & 0x3f) | 0x80;
    var h = Array.prototype.map.call(b, function (x) { return (x + 0x100).toString(16).slice(1); }).join("");
    return h.slice(0, 8) + "-" + h.slice(8, 12) + "-" + h.slice(12, 16) + "-" + h.slice(16, 20) + "-" + h.slice(20);
  }

  function browserInfo() {
    var ua = global.navigator ? navigator.userAgent : "";
    var m;
    var name = "Browser", version = "";
    if ((m = ua.match(/Edg\/([\d.]+)/))) { name = "Edge"; version = m[1]; }
    else if ((m = ua.match(/Firefox\/([\d.]+)/))) { name = "Firefox"; version = m[1]; }
    else if ((m = ua.match(/Chrome\/([\d.]+)/))) { name = "Chrome"; version = m[1]; }
    else if ((m = ua.match(/Version\/([\d.]+).*Safari/))) { name = "Safari"; version = m[1]; }
    var os = /iPhone|iPad/.test(ua) ? "iOS" : /Android/.test(ua) ? "Android" : /Mac OS X/.test(ua) ? "macOS" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
    return { name: name, version: version.split(".").slice(0, 2).join("."), os: os };
  }

  function Snag(options) {
    if (!options || !options.url || !options.apiKey) throw new Error("Snag.init needs { url, apiKey }");
    this.options = Object.assign({
      app: { name: (global.document && document.title) || "Web" },
      redaction: "standard", // "standard", "strict" or "off"
      button: true,
      kinds: ["bug", "feedback", "feature"],
      breadcrumbs: true,
      console: true,
      contact: "",
      tags: [],
      maxScreenshotPixels: 2400,
      context: null, // () => ({ key: "value" })
      text: {},
    }, options);
    this.text = Object.assign({}, DEFAULT_TEXT, this.options.text);
    this.endpoint = String(this.options.url).replace(/\/+$/, "") + "/api/v1/reports";
    this.crumbs = new Ring(100);
    this.logs = new Ring(300);
    this.custom = {};
    if (global.document) {
      this._instrument();
      if (this.options.button) this._mountButton();
      this._flushQueue();
      var self = this;
      global.addEventListener("online", function () { self._flushQueue(); });
    }
  }

  Snag.prototype.breadcrumb = function (kind, message, data) {
    this.crumbs.push({ at: new Date().toISOString(), kind: kind, message: String(message).slice(0, 500), data: data || {} });
  };

  Snag.prototype.setContext = function (key, value) {
    if (value == null) delete this.custom[key]; else this.custom[key] = String(value);
  };

  Snag.prototype._instrument = function () {
    var self = this, opts = this.options;
    if (opts.console) {
      ["log", "info", "warn", "error", "debug"].forEach(function (level) {
        var original = console[level];
        if (typeof original !== "function" || original.__snag) return;
        var wrapped = function () {
          try {
            var line = Array.prototype.map.call(arguments, formatArg).join(" ");
            self.logs.push(new Date().toISOString().slice(11, 23) + " [" + level + "] " + line.slice(0, 4000));
            if (level === "error" && opts.breadcrumbs) self.breadcrumb("log", line.slice(0, 200));
          } catch (e) {}
          return original.apply(this, arguments);
        };
        wrapped.__snag = true;
        console[level] = wrapped;
      });
      global.addEventListener("error", function (e) {
        self.logs.push(new Date().toISOString().slice(11, 23) + " [uncaught] " + (e.error ? formatArg(e.error) : e.message));
        if (opts.breadcrumbs) self.breadcrumb("log", "Uncaught: " + (e.message || "error"));
      });
      global.addEventListener("unhandledrejection", function (e) {
        self.logs.push(new Date().toISOString().slice(11, 23) + " [unhandled rejection] " + formatArg(e.reason));
        if (opts.breadcrumbs) self.breadcrumb("log", "Unhandled rejection: " + formatArg(e.reason).slice(0, 160));
      });
    }
    if (!opts.breadcrumbs) return;

    var page = function () { self.breadcrumb("navigation", sanitizeURL(global.location.href)); };
    page();
    ["pushState", "replaceState"].forEach(function (name) {
      var original = history[name];
      if (original.__snag) return;
      var wrapped = function () {
        var result = original.apply(this, arguments);
        page();
        return result;
      };
      wrapped.__snag = true;
      history[name] = wrapped;
    });
    global.addEventListener("popstate", page);
    global.addEventListener("hashchange", page);
    document.addEventListener("visibilitychange", function () {
      self.breadcrumb("lifecycle", document.visibilityState === "hidden" ? "Page hidden" : "Page visible");
    });
    document.addEventListener("click", function (e) {
      if (self._host && e.composedPath && e.composedPath().indexOf(self._host) >= 0) return;
      self.breadcrumb("action", "Clicked " + describeElement(e.target));
    }, true);
    document.addEventListener("submit", function (e) {
      if (e.target && e.target.getRootNode && e.target.getRootNode() === self._root) return;
      self.breadcrumb("action", "Submitted " + describeElement(e.target));
    }, true);

    var endpoint = this.endpoint;
    var record = function (method, url, status, started, failed) {
      if (String(url).indexOf(endpoint) === 0) return;
      var target = sanitizeURL(url), ms = Math.round(performance.now() - started);
      var data = { method: method, url: target, ms: String(ms) };
      var message = method + " " + target;
      if (failed) { data.error = "network"; message += " → failed"; } else { data.status = String(status); message += " → " + status; }
      self.breadcrumb("network", message + " · " + ms + " ms", data);
    };
    if (global.fetch && !global.fetch.__snag) {
      var originalFetch = global.fetch;
      var wrappedFetch = function (input, init) {
        var method = ((init && init.method) || (input && input.method) || "GET").toUpperCase();
        var url = typeof input === "string" ? input : input && input.url ? input.url : String(input);
        var started = performance.now();
        return originalFetch.apply(this, arguments).then(function (response) {
          record(method, url, response.status, started, false);
          return response;
        }, function (error) {
          record(method, url, 0, started, true);
          throw error;
        });
      };
      wrappedFetch.__snag = true;
      global.fetch = wrappedFetch;
    }
    if (global.XMLHttpRequest && !XMLHttpRequest.prototype.open.__snag) {
      var open = XMLHttpRequest.prototype.open, send = XMLHttpRequest.prototype.send;
      XMLHttpRequest.prototype.open = function (method, url) {
        this.__snag = { method: String(method).toUpperCase(), url: url };
        return open.apply(this, arguments);
      };
      XMLHttpRequest.prototype.open.__snag = true;
      XMLHttpRequest.prototype.send = function () {
        var info = this.__snag, xhr = this;
        if (info) {
          var started = performance.now();
          xhr.addEventListener("loadend", function () { record(info.method, info.url, xhr.status, started, xhr.status === 0); });
        }
        return send.apply(this, arguments);
      };
    }
  };

  // ---------------------------------------------------------------------------------------------
  // UI

  Snag.prototype._mountButton = function () {
    var self = this;
    var mount = function () {
      self._ensureRoot();
      var button = document.createElement("button");
      button.className = "launch";
      button.type = "button";
      button.textContent = self.text.button;
      button.setAttribute("aria-haspopup", "dialog");
      button.addEventListener("click", function () { self.open(); });
      self._root.appendChild(button);
      self._launch = button;
    };
    if (document.body) mount(); else document.addEventListener("DOMContentLoaded", mount);
  };

  Snag.prototype._ensureRoot = function () {
    if (this._root) return;
    var host = document.createElement("snag-feedback");
    host.setAttribute("data-snag-private", "");
    document.body.appendChild(host);
    this._host = host;
    this._root = host.attachShadow({ mode: "open" });
    var style = document.createElement("style");
    style.textContent = STYLE;
    this._root.appendChild(style);
    if (this.options.accentColor) host.style.setProperty("--snag-accent", this.options.accentColor);
  };

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === "text") node.textContent = attrs[k];
      else if (attrs[k] === true) node.setAttribute(k, "");
      else if (attrs[k] !== false && attrs[k] != null) node.setAttribute(k, attrs[k]);
    });
    (children || []).forEach(function (c) { if (c) node.appendChild(c); });
    return node;
  }

  /** Opens the report form. `kind` preselects "bug", "feedback" or "feature" (an idea). */
  Snag.prototype.open = function (preset) {
    preset = preset || {};
    this._ensureRoot();
    var self = this, t = this.text;
    if (this._dialog) this._dialog.remove();
    var shot = null;
    var kinds = this.options.kinds.length > 1 ? el("fieldset", { class: "kinds", style: "border:0;padding:0;margin:0" }, [el("legend", { style: "position:absolute;left:-9999px", text: "Type" })].concat(
      this.options.kinds.map(function (k, i) {
        return el("label", {}, [el("input", { type: "radio", name: "kind", value: k, checked: (preset.kind ? preset.kind === k : i === 0) }), el("span", { text: t[k] || k })]);
      }))) : null;
    var title = el("input", { type: "text", name: "title", maxlength: "300", "aria-label": t.summary, placeholder: t.summary, autocomplete: "off" });
    var message = el("textarea", { name: "message", "aria-label": t.message, placeholder: t.message, required: true });
    var email = el("input", { type: "email", name: "contact", "aria-label": t.email, placeholder: t.email, autocomplete: "email", "aria-describedby": "snag-email-hint" });
    email.value = this.options.contact || safeStorage("get", "snag.contact") || "";
    var shotRow = el("div", { class: "row" });
    var error = el("p", { class: "error", role: "alert" });
    var send = el("button", { type: "submit", class: "primary", text: t.send });
    var cancel = el("button", { type: "button", text: t.cancel });
    var renderShot = function () {
      shotRow.textContent = "";
      if (shot) {
        var img = el("img", { alt: t.screenshot });
        img.src = URL.createObjectURL(shot);
        var remove = el("button", { type: "button", text: t.removeScreenshot });
        remove.addEventListener("click", function () { shot = null; renderShot(); });
        shotRow.appendChild(el("div", { class: "shot" }, [img]));
        shotRow.appendChild(remove);
      } else if (navigator.mediaDevices && navigator.mediaDevices.getDisplayMedia) {
        var add = el("button", { type: "button", text: t.screenshot, "aria-describedby": "snag-shot-hint" });
        add.addEventListener("click", function () {
          error.textContent = "";
          dialog.close();
          self.captureScreenshot().then(function (blob) { shot = blob; }, function (e) {
            if (e && e.snagWrongSurface) error.textContent = t.wrongSurface;
          }).then(function () { dialog.showModal(); renderShot(); });
        });
        shotRow.appendChild(add);
        shotRow.appendChild(el("p", { class: "hint", id: "snag-shot-hint", text: t.screenshotHint }));
      }
    };
    var form = el("form", { method: "dialog", "aria-labelledby": "snag-title", novalidate: true }, [
      el("h2", { id: "snag-title", text: t.title }), kinds, title, message, email,
      el("p", { class: "hint", id: "snag-email-hint", text: t.emailHint }),
      shotRow, el("p", { class: "hint", text: t.privacy }), error,
      el("div", { class: "actions" }, [cancel, send]),
    ]);
    var dialog = el("dialog", { "aria-labelledby": "snag-title" }, [form]);
    this._root.appendChild(dialog);
    this._dialog = dialog;
    renderShot();
    cancel.addEventListener("click", function () { dialog.close(); });
    dialog.addEventListener("close", function () { if (self._launch && !dialog.__capturing) self._launch.focus(); });
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      if (!message.value.trim() && !title.value.trim()) { error.textContent = t.message; message.focus(); return; }
      var kindInput = form.querySelector("input[name=kind]:checked");
      send.disabled = true;
      send.textContent = t.sending;
      safeStorage("set", "snag.contact", email.value.trim());
      self.report({
        kind: kindInput ? kindInput.value : self.options.kinds[0],
        title: title.value.trim(), message: message.value.trim(), contact: email.value.trim(),
        screenshot: shot,
      }).then(function (result) {
        form.replaceWith(el("div", { class: "done", role: "status" }, [
          el("p", { text: result.queued ? t.queued : t.sent }),
          (function () { var b = el("button", { type: "button", class: "primary", text: t.close }); b.addEventListener("click", function () { dialog.close(); }); return b; })(),
        ]));
        dialog.querySelector(".done button").focus();
      }, function (err) {
        send.disabled = false;
        send.textContent = t.send;
        error.textContent = t.failed + (err && err.message ? err.message : err);
      });
    });
    dialog.showModal();
    title.focus();
    return dialog;
  };

  /**
   * Asks the browser to share this tab, grabs one frame, and blacks out password fields,
   * card fields and anything marked `data-snag-private` (in strict mode, every form field).
   * Rejects when the user shares something other than this tab, since its private areas can't
   * be located.
   */
  Snag.prototype.captureScreenshot = function () {
    var self = this;
    var privateRects = function () {
      var selector = "[data-snag-private], .snag-private, input[type=password], input[autocomplete^=cc-], input[autocomplete=one-time-code]";
      if (self.options.redaction === "strict") selector += ", input, textarea, select, [contenteditable=''], [contenteditable=true]";
      return Array.prototype.map.call(document.querySelectorAll(selector), function (node) {
        return node === self._host ? null : node.getBoundingClientRect();
      }).filter(function (r) { return r && r.width > 0 && r.height > 0; });
    };
    return navigator.mediaDevices.getDisplayMedia({
      video: { displaySurface: "browser" }, audio: false, preferCurrentTab: true, selfBrowserSurface: "include", surfaceSwitching: "exclude",
    }).then(function (stream) {
      var track = stream.getVideoTracks()[0];
      var settings = track.getSettings ? track.getSettings() : {};
      var video = document.createElement("video");
      video.muted = true;
      video.playsInline = true;
      video.srcObject = stream;
      return video.play().then(function () {
        return new Promise(function (resolve) { setTimeout(resolve, 250); });
      }).then(function () {
        var w = video.videoWidth, h = video.videoHeight;
        var stop = function () { stream.getTracks().forEach(function (tr) { tr.stop(); }); };
        // Only this tab can be redacted: its frame matches the viewport's shape.
        var viewportRatio = global.innerWidth / global.innerHeight;
        if ((settings.displaySurface && settings.displaySurface !== "browser") || Math.abs(w / h - viewportRatio) > 0.04) {
          stop();
          var e = new Error("not this tab");
          e.snagWrongSurface = true;
          throw e;
        }
        var scale = Math.min(1, self.options.maxScreenshotPixels / Math.max(w, h));
        var canvas = document.createElement("canvas");
        canvas.width = Math.round(w * scale);
        canvas.height = Math.round(h * scale);
        var ctx = canvas.getContext("2d");
        ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
        stop();
        var k = canvas.width / global.innerWidth;
        ctx.fillStyle = "#000";
        privateRects().forEach(function (r) {
          ctx.fillRect(Math.floor(r.left * k) - 2, Math.floor(r.top * k) - 2, Math.ceil(r.width * k) + 4, Math.ceil(r.height * k) + 4);
        });
        return new Promise(function (resolve) { canvas.toBlob(resolve, "image/png"); });
      });
    });
  };

  // ---------------------------------------------------------------------------------------------
  // Sending

  Snag.prototype._context = function () {
    var b = browserInfo();
    var custom = Object.assign({}, this.custom);
    if (typeof this.options.context === "function") {
      try { Object.assign(custom, this.options.context() || {}); } catch (e) {}
    }
    custom.page = sanitizeURL(global.location.href);
    Object.keys(custom).forEach(function (k) { custom[k] = String(custom[k]); });
    return {
      app: { name: this.options.app.name || "Web", version: this.options.app.version, build: this.options.app.build, bundleIdentifier: global.location.host },
      device: {
        model: "Web", system: b.name + (b.os ? " on " + b.os : ""), systemVersion: b.version,
        locale: navigator.language, screen: screen.width + "×" + screen.height + " @" + (global.devicePixelRatio || 1) + "x",
        viewport: global.innerWidth + "×" + global.innerHeight,
        colorScheme: global.matchMedia && matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light",
      },
      custom: custom,
    };
  };

  /**
   * Sends a report without the form. Resolves with { id, issue, url } (or { queued: true } when
   * offline: it's kept and retried).
   */
  Snag.prototype.report = function (report) {
    var self = this, level = this.options.redaction;
    var id = uuid();
    var files = [];
    var add = function (role, filename, type, blob) {
      files.push({ entry: { id: uuid(), role: role, filename: filename, contentType: type, size: blob.size, part: "attachment" + files.length }, blob: blob });
    };
    if (report.screenshot) add("screenshot", "screenshot.png", "image/png", report.screenshot);
    if (this.logs.items.length) add("log", "console.txt", "text/plain", new Blob([redact(this.logs.items.join("\n"), level)], { type: "text/plain" }));
    if (this.crumbs.items.length) {
      var crumbs = this.crumbs.items.map(function (c) { return Object.assign({}, c, { message: redact(c.message, level) }); });
      add("log", "breadcrumbs.json", "application/json", new Blob([JSON.stringify(crumbs, null, 2)], { type: "application/json" }));
    }
    var context = this._context();
    Object.keys(context.custom).forEach(function (k) { context.custom[k] = redact(context.custom[k], level); });
    var manifest = {
      id: id, createdAt: new Date().toISOString(), kind: report.kind || "bug",
      title: redact(report.title || "", level), message: redact(report.message || "", level),
      contact: report.contact || undefined, tags: (this.options.tags || []).concat(report.tags || [], ["web"]),
      context: context, attachments: files.map(function (f) { return f.entry; }),
    };
    return this._send(manifest, files).catch(function (err) {
      if (err && err.retryable) {
        return self._queue(manifest, files).then(function () { return { id: id, queued: true }; });
      }
      throw err;
    });
  };

  Snag.prototype._send = function (manifest, files) {
    var body = new FormData();
    body.append("report", new Blob([JSON.stringify(manifest)], { type: "application/json" }), "report.json");
    files.forEach(function (f) { body.append(f.entry.part, f.blob, f.entry.filename); });
    return fetch(this.endpoint, {
      method: "POST", body: body, mode: "cors", credentials: "omit",
      headers: { Authorization: "Bearer " + this.options.apiKey, "Idempotency-Key": manifest.id },
    }).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (json) {
        if (response.ok) return { id: json.id, issue: json.issue, url: json.url };
        var err = new Error(json.error || "HTTP " + response.status);
        err.retryable = response.status === 429 || response.status >= 500;
        throw err;
      });
    }, function (networkError) {
      var err = new Error(networkError.message || "network error");
      err.retryable = true;
      throw err;
    });
  };

  // Offline reports wait in localStorage (up to ~3 MB) and go out on the next load or `online`.
  var QUEUE_KEY = "snag.queue";

  function safeStorage(op, key, value) {
    try {
      if (op === "get") return global.localStorage.getItem(key);
      if (op === "set") global.localStorage.setItem(key, value);
      if (op === "remove") global.localStorage.removeItem(key);
    } catch (e) { return null; }
  }

  function blobToDataURL(blob) {
    return new Promise(function (resolve, reject) {
      var r = new FileReader();
      r.onload = function () { resolve(r.result); };
      r.onerror = reject;
      r.readAsDataURL(blob);
    });
  }

  Snag.prototype._queue = function (manifest, files) {
    return Promise.all(files.map(function (f) { return blobToDataURL(f.blob); })).then(function (urls) {
      var queue = JSON.parse(safeStorage("get", QUEUE_KEY) || "[]");
      queue.push({ manifest: manifest, files: files.map(function (f, i) { return { entry: f.entry, data: urls[i] }; }) });
      var json = JSON.stringify(queue);
      while (json.length > 3e6 && queue.length > 1) { queue.shift(); json = JSON.stringify(queue); }
      if (json.length > 3e6) {
        // Too big to keep with its screenshot: keep the text.
        queue[0].files = queue[0].files.filter(function (f) { return f.entry.role !== "screenshot"; });
        queue[0].manifest.attachments = queue[0].files.map(function (f) { return f.entry; });
        json = JSON.stringify(queue);
      }
      safeStorage("set", QUEUE_KEY, json);
    });
  };

  Snag.prototype._flushQueue = function () {
    var self = this;
    if (this._flushing) return this._flushing;
    var queue;
    try { queue = JSON.parse(safeStorage("get", QUEUE_KEY) || "[]"); } catch (e) { queue = []; }
    if (!queue.length) return Promise.resolve();
    this._flushing = queue.reduce(function (chain, item) {
      return chain.then(function (remaining) {
        var files = item.files.map(function (f) { return { entry: f.entry, blob: dataURLToBlob(f.data) }; });
        return self._send(item.manifest, files).then(function () { return remaining; }, function (err) {
          return err.retryable ? remaining.concat([item]) : remaining; // drop what the server refuses
        });
      });
    }, Promise.resolve([])).then(function (remaining) {
      if (remaining.length) safeStorage("set", QUEUE_KEY, JSON.stringify(remaining)); else safeStorage("remove", QUEUE_KEY);
      self._flushing = null;
    });
    return this._flushing;
  };

  function dataURLToBlob(url) {
    var parts = url.split(","), type = parts[0].replace(/^data:|;base64$/g, "");
    var bin = atob(parts[1] || ""), bytes = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    return new Blob([bytes], { type: type });
  }

  // ---------------------------------------------------------------------------------------------

  var instance = null;
  var api = {
    version: VERSION,
    /** Starts Snag on this page. Call once. */
    init: function (options) { instance = new Snag(options); return instance; },
    open: function (preset) { return need().open(preset); },
    report: function (report) { return need().report(report); },
    breadcrumb: function (kind, message, data) { if (instance) instance.breadcrumb(kind, message, data); },
    setContext: function (key, value) { if (instance) instance.setContext(key, value); },
    // For tests and advanced use.
    _redact: redact, _sanitizeURL: sanitizeURL, _describeElement: describeElement, Snag: Snag,
  };
  function need() {
    if (!instance) throw new Error("Call Snag.init({ url, apiKey }) first");
    return instance;
  }

  if (typeof module === "object" && module.exports) module.exports = api;
  else global.Snag = api;
})(typeof globalThis !== "undefined" ? globalThis : this);
