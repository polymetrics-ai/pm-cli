(() => {
  "use strict";

  const status = document.getElementById("status");
  const nextActions = document.getElementById("next-actions");
  const safePath = window.location.pathname;
  let code = "";
  let state = "";
  let requestBody = "";

  window.addEventListener("pagehide", () => {
    code = "";
    state = "";
    requestBody = "";
  });

  const complete = async () => {
    try {
      let callback = new URL(window.location.href);
      code = callback.searchParams.get("code") || "";
      state = callback.searchParams.get("state") || "";
      const providerDenied = callback.searchParams.has("error");

      // Remove one-time provider values before any other navigation or request.
      window.history.replaceState(null, document.title, safePath);
      callback = null;
      if (providerDenied || code === "" || state === "") {
        throw new Error("callback incomplete");
      }

      requestBody = JSON.stringify({code, state});
      code = "";
      state = "";
      const response = await fetch("/v1/sessions/browser", {
        method: "POST",
        credentials: "same-origin",
        headers: {"Content-Type": "application/json"},
        body: requestBody,
      });
      requestBody = "";
      if (!response.ok) {
        throw new Error("callback rejected");
      }
      status.textContent = "Sign in complete.";
      nextActions.classList.remove("hidden");
    } catch (_) {
      code = "";
      state = "";
      requestBody = "";
      if (window.location.search !== "" || window.location.hash !== "") {
        window.location.replace(safePath);
        return;
      }
      status.textContent = "Sign in could not be completed. Return to the sign-in page and try again.";
    }
  };

  void complete();
})();
