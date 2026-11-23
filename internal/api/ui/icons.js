import { h } from "/vue.js";

// Icons are drawn as SVG elements, never loaded as images: the page's
// Content-Security-Policy allows no remote or data: images.
const paths = {
  chat: ["M4 5h16v11H10l-6 4V5Z"],
  work: ["M4 8h16v11H4V8Z", "M9 8V5h6v3", "M4 13h16"],
  inbox: ["M4 13l3-8h10l3 8v6H4v-6Z", "M4 13h5l1 2h4l1-2h5"],
  tools: ["M5 7h7", "M18 7h1", "M5 17h1", "M12 17h7", "M15 7m-3 0a3 3 0 1 0 6 0a3 3 0 1 0-6 0", "M9 17m-3 0a3 3 0 1 0 6 0a3 3 0 1 0-6 0"],
  activity: ["M3 12h4l3-7 4 14 3-7h4"],
  stop: ["M8 3h8l5 5v8l-5 5H8l-5-5V8l5-5Z", "M9 9l6 6", "M15 9l-6 6"],
  signout: ["M10 4H5v16h5", "M15 8l4 4-4 4", "M19 12H9"],
  send: ["M12 19V5", "M6 11l6-6 6 6"],
  plus: ["M12 5v14", "M5 12h14"],
  proposal: ["M7 3h7l4 4v14H7V3Z", "M14 3v4h4", "M12 11v6", "M9 14h6"],
  approval: ["M12 3l7 3v6c0 4-3 7-7 9-4-2-7-5-7-9V6l7-3Z", "M9 12l2 2 4-4"],
  question: ["M12 12m-9 0a9 9 0 1 0 18 0a9 9 0 1 0-18 0", "M9.5 9.5a2.5 2.5 0 1 1 3.6 2.2c-.7.4-1.1 1-1.1 1.8", "M12 17v.5"],
  notification: ["M6 16v-5a6 6 0 0 1 12 0v5l2 2H4l2-2Z", "M10 21h4"],
  report: ["M12 12m-9 0a9 9 0 1 0 18 0a9 9 0 1 0-18 0", "M8 12.5l2.5 2.5 5.5-6"],
  arrow: ["M5 12h14", "M13 6l6 6-6 6"],
};

export function icon(name, cls) {
  return h("svg", { class: "icon" + (cls ? " " + cls : ""), viewBox: "0 0 24 24", "aria-hidden": "true", focusable: "false" },
    (paths[name] || []).map((d) => h("path", { d })));
}
