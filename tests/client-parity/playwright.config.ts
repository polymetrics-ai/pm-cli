import { defineConfig } from "@playwright/test";
import { isAbsolute } from "node:path";

const executablePath = process.env.PM_BROWSER_EXECUTABLE;
const certificatePin = process.env.PM_PARITY_CERTIFICATE_SPKI;
const outputDir = process.env.PM_PARITY_OUTPUT_DIR;
const reportPath = process.env.PM_PARITY_REPORT;
const testDirectory = process.env.PM_PARITY_TEST_DIR;
if (!executablePath || !isAbsolute(executablePath) || !testDirectory || !isAbsolute(testDirectory) ||
    !outputDir || !isAbsolute(outputDir) || !reportPath || !isAbsolute(reportPath) ||
    !certificatePin || !/^[A-Za-z0-9+/]{43}=$/.test(certificatePin)) {
  throw new Error("Go-owned browser path, test directory, certificate pin and output paths are required");
}

export default defineConfig({
  testDir: testDirectory,
  testMatch: "browser.ts",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  globalTimeout: 180_000,
  expect: { timeout: 5_000 },
  outputDir,
  reporter: [["json", { outputFile: reportPath }]],
  use: {
    browserName: "chromium",
    headless: true,
    ignoreHTTPSErrors: false,
    trace: "off",
    video: "off",
    screenshot: "off",
    launchOptions: {
      executablePath,
      args: [
        `--ignore-certificate-errors-spki-list=${certificatePin}`,
        // Fork/exec directly: the confined Linux runner denies zygote capset.
        // Retain Playwright's existing sandbox policy and separate processes.
        ...(process.platform === "linux" ? ["--no-zygote"] : []),
      ],
    },
  },
});
