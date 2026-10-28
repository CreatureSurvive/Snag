// Passkey sign-in (login page) and registration (account page). No dependencies.
(function () {
  "use strict";
  function b64(buf) {
    var bytes = new Uint8Array(buf), s = "";
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  function unb64(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }
  function post(url, body, csrf) {
    var headers = { "Content-Type": "application/json" };
    if (csrf) headers["X-Snag-CSRF"] = csrf;
    return fetch(url, { method: "POST", headers: headers, body: JSON.stringify(body || {}), credentials: "same-origin" }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (json) {
        if (!r.ok) throw new Error(json.error || "HTTP " + r.status);
        return json;
      });
    });
  }
  function showError(e) {
    var el = document.getElementById("passkey-error");
    if (!el) return;
    el.hidden = false;
    el.textContent = e && e.name === "NotAllowedError" ? "Cancelled." : (e && e.message) || String(e);
  }
  var supported = !!(window.PublicKeyCredential && navigator.credentials);

  var login = document.getElementById("passkey-login");
  if (login) {
    if (!supported) { login.hidden = true; }
    login.addEventListener("click", function () {
      login.disabled = true;
      post("/login/passkey/begin").then(function (o) {
        return navigator.credentials.get({ publicKey: {
          challenge: unb64(o.challenge), rpId: o.rpId, timeout: o.timeout, userVerification: o.userVerification,
          allowCredentials: (o.allowCredentials || []).map(function (c) { return { type: c.type, id: unb64(c.id) }; }),
        } });
      }).then(function (cred) {
        return post("/login/passkey/finish", {
          id: cred.id, clientDataJSON: b64(cred.response.clientDataJSON),
          authenticatorData: b64(cred.response.authenticatorData), signature: b64(cred.response.signature),
          next: login.getAttribute("data-next") || "/",
        });
      }).then(function (r) { location.href = r.next || "/"; }, function (e) { login.disabled = false; showError(e); });
    });
  }

  var add = document.getElementById("passkey-add");
  if (add) {
    if (!supported) { add.disabled = true; showError(new Error("This browser doesn't support passkeys.")); }
    var csrf = add.getAttribute("data-csrf");
    add.addEventListener("click", function () {
      add.disabled = true;
      post("/account/passkeys/begin", {}, csrf).then(function (o) {
        return navigator.credentials.create({ publicKey: {
          challenge: unb64(o.challenge), rp: o.rp, timeout: o.timeout, attestation: o.attestation,
          user: { id: unb64(o.user.id), name: o.user.name, displayName: o.user.displayName },
          pubKeyCredParams: o.pubKeyCredParams, authenticatorSelection: o.authenticatorSelection,
          excludeCredentials: (o.excludeCredentials || []).map(function (c) { return { type: c.type, id: unb64(c.id) }; }),
        } });
      }).then(function (cred) {
        return post("/account/passkeys/finish", {
          name: document.getElementById("passkey-name").value,
          clientDataJSON: b64(cred.response.clientDataJSON), attestationObject: b64(cred.response.attestationObject),
        }, csrf);
      }).then(function () { location.href = "/account?flash=Passkey+added"; }, function (e) { add.disabled = false; showError(e); });
    });
  }
})();
