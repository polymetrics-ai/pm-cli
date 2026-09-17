(() => {
  "use strict";

  const form = document.getElementById("login-form");
  const reauthenticate = document.getElementById("reauthenticate");
  const status = document.getElementById("status");
  const submit = form.querySelector("button[type='submit']");

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    submit.disabled = true;
    status.textContent = "Contacting the configured identity provider…";

    try {
      const response = await fetch("/v1/sessions/browser/challenges", {
        method: "POST",
        credentials: "same-origin",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({reauthenticate: reauthenticate.checked}),
      });
      if (!response.ok) {
        throw new Error("challenge unavailable");
      }
      const challenge = await response.json();
      if (typeof challenge.authorization_url !== "string") {
        throw new Error("challenge invalid");
      }
      const destination = new URL(challenge.authorization_url);
      if (destination.protocol !== "https:") {
        throw new Error("challenge target invalid");
      }
      window.location.replace(destination.href);
    } catch (_) {
      status.textContent = "Sign in could not be started. Try again or contact your PM operator.";
      submit.disabled = false;
    }
  });
})();
