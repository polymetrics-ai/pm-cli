(() => {
  "use strict";

  const csrfCookieName = "__Host-pm_csrf";
  const form = document.getElementById("device-form");
  const challengeInput = document.getElementById("challenge-id");
  const codeInput = document.getElementById("user-code");
  const status = document.getElementById("status");
  const buttons = Array.from(form.querySelectorAll("button"));

  const clearDeviceFields = () => { form.reset(); };
  window.addEventListener("pagehide", clearDeviceFields);
  window.addEventListener("pageshow", clearDeviceFields);

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

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const decision = event.submitter && event.submitter.dataset.decision;
    if (decision !== "approve" && decision !== "deny") {
      status.textContent = "Choose approve or deny.";
      return;
    }
    const csrf = readCookie(csrfCookieName);
    if (csrf === "") {
      form.reset();
      status.textContent = "Sign in before deciding a device login.";
      return;
    }

    let requestBody = JSON.stringify({
      challenge_id: challengeInput.value,
      user_code: codeInput.value,
    });
    form.reset();
    buttons.forEach((button) => { button.disabled = true; });
    status.textContent = decision === "approve" ? "Approving device…" : "Denying device…";

    try {
      const response = await fetch(`/v1/sessions/device/${decision}`, {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": csrf,
        },
        body: requestBody,
      });
      requestBody = "";
      if (!response.ok) {
        throw new Error("decision rejected");
      }
      status.textContent = decision === "approve" ? "Device login approved." : "Device login denied.";
    } catch (_) {
      requestBody = "";
      status.textContent = "The device decision was not accepted. Check the values, session, and challenge status.";
    } finally {
      buttons.forEach((button) => { button.disabled = false; });
    }
  });
})();
