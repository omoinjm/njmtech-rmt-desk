// Entry point: pick the view for how we're running.
"use strict";

import { api } from "./platform.js";
import { start, enterHome } from "./home.js";
import { setOnLeave } from "./session.js";
import { openSetup } from "./setup.js";
import { toast, errText } from "./ui.js";

setOnLeave(enterHome);
try {
  const info = await api.info();
  if (info.mode === "setup") openSetup({ inProcess: false });
  else await start(info);
} catch (e) {
  toast("Startup failed: " + errText(e), "error");
}
