import { configure } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";

// Role queries on a whole page are slow in jsdom; with every test file running in parallel the
// default 1 s for findBy* is too tight and made a test flaky (it passed alone).
configure({ asyncUtilTimeout: 3000 });

// jsdom has no modal dialogs: enough of the native API for a dialog to open, close and be found.
HTMLDialogElement.prototype.showModal ??= function (this: HTMLDialogElement) {
  this.setAttribute("open", "");
};
HTMLDialogElement.prototype.close ??= function (this: HTMLDialogElement) {
  this.removeAttribute("open");
};
