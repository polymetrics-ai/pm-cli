(() => {
  "use strict";

  const csrfCookieName = "__Host-pm_csrf";
  const maximumCredentialBytes = 64 * 1024;
  const form = document.getElementById("credential-form");
  const workspaceInput = document.getElementById("workspace-id");
  const connectionInput = document.getElementById("connection-id");
  const scopesInput = document.getElementById("scopes");
  const credentialInput = document.getElementById("credential-value");
  const reauthenticate = document.getElementById("reauthenticate");
  const status = document.getElementById("status");
  const buttons = Array.from(form.querySelectorAll("button"));

  const clearCredentialField = () => { credentialInput.value = ""; };
  window.addEventListener("pagehide", clearCredentialField);
  window.addEventListener("pageshow", clearCredentialField);

  const readCookie = (name) => {
    const prefix = `${name}=`;
    for (const part of document.cookie.split(";")) {
      const value = part.trim();
      if (value.startsWith(prefix)) {
        try {
          return decodeURIComponent(value.slice(prefix.length));
        } catch (_) {
          return "";
        }
      }
    }
    return "";
  };

  const base64 = (bytes) => {
    let binary = "";
    for (let offset = 0; offset < bytes.length; offset += 0x4000) {
      binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x4000));
    }
    const encoded = window.btoa(binary);
    binary = "";
    return encoded;
  };

  const newIdempotencyKey = () => {
    if (typeof window.crypto.randomUUID === "function") {
      return window.crypto.randomUUID();
    }
    const bytes = new Uint8Array(16);
    window.crypto.getRandomValues(bytes);
    const value = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
    bytes.fill(0);
    return value;
  };

  reauthenticate.addEventListener("click", async () => {
    credentialInput.value = "";
    buttons.forEach((button) => { button.disabled = true; });
    status.textContent = "Starting reauthentication…";
    try {
      const response = await fetch("/v1/sessions/browser/challenges", {
        method: "POST",
        credentials: "same-origin",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({reauthenticate: true}),
      });
      if (!response.ok) {
        throw new Error("reauthentication unavailable");
      }
      const challenge = await response.json();
      if (typeof challenge.authorization_url !== "string") {
        throw new Error("reauthentication challenge invalid");
      }
      const destination = new URL(challenge.authorization_url);
      if (destination.protocol !== "https:") {
        throw new Error("reauthentication target invalid");
      }
      window.location.replace(destination.href);
    } catch (_) {
      status.textContent = "Reauthentication could not be started.";
      buttons.forEach((button) => { button.disabled = false; });
    }
  });

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const csrf = readCookie(csrfCookieName);
    if (csrf === "") {
      credentialInput.value = "";
      status.textContent = "Sign in and reauthenticate before storing a credential.";
      return;
    }

    const workspaceID = workspaceInput.value;
    const connectionID = connectionInput.value;
    const scopes = Array.from(new Set(
      scopesInput.value.split(/\r?\n/u).map((value) => value.trim()).filter((value) => value !== ""),
    )).sort();
    let credentialText = credentialInput.value;
    const credentialBytes = new TextEncoder().encode(credentialText);
    credentialText = "";
    credentialInput.value = "";
    if (credentialBytes.length === 0 || credentialBytes.length > maximumCredentialBytes) {
      credentialBytes.fill(0);
      status.textContent = "The credential must contain between 1 and 65,536 UTF-8 bytes.";
      return;
    }

    let encodedCredential = base64(credentialBytes);
    credentialBytes.fill(0);
    let requestBody = JSON.stringify({credential_value: encodedCredential, scopes});
    encodedCredential = "";
    buttons.forEach((button) => { button.disabled = true; });
    status.textContent = "Storing credential…";

    try {
      const endpoint = `/admin/v1/workspaces/${encodeURIComponent(workspaceID)}/connections/${encodeURIComponent(connectionID)}/credentials`;
      const response = await fetch(endpoint, {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": newIdempotencyKey(),
          "X-CSRF-Token": csrf,
        },
        body: requestBody,
      });
      requestBody = "";
      if (!response.ok) {
        if (response.status === 401 || response.status === 403) {
          status.textContent = "Current authorization or recent reauthentication is required.";
          return;
        }
        status.textContent = "The credential was not accepted. Check the connection, scope, and current authorization.";
        return;
      }
      status.textContent = "Credential stored. No credential value can be read back.";
    } catch (_) {
      requestBody = "";
      status.textContent = "The storage outcome could not be confirmed. Do not resubmit until the connection state is checked.";
    } finally {
      requestBody = "";
      buttons.forEach((button) => { button.disabled = false; });
    }
  });
})();
