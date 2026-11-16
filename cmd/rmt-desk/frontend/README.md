The UI lives in internal/viewer/web/static and is embedded by
web.Assets() (shared with `rmt ui`). This directory only exists because the
Wails CLI expects a frontend dir; it has no build step.
