# 0026 Image input: an image part in messages, and how each engine receives it

- Status: Accepted (2026-10-27)
- Date: 2026-10-26
- Related: design section 2.2 (`kc_message`), ADR 0002 (ACP), 0007 (Claude Code engine), 0014 (ACP engine), invariants 5 and 6

## Context

The user sometimes wants to show a screenshot (an error screen, a design) along with a request. Claude Code, opencode and codex all accept images as input.

Today a message part (`msg.Part`) is one of only 3 things: text, a tool call, or a tool result. And user input accepts only text.
- As an ACP agent, it answers `promptCapabilities.image: false`.
- `Runtime.Send` also accepts only text.

`kc_message` is written once, keyed by the hash of its content, and the input of a model call is a list of hashes (invariant 5). Images are much larger than messages, so embedding them in the message body would make every read of the history heavy.

## Decision

1. **Add an `image` part type.** Add `Type: "image"`, `MediaType` (`image/png`, `image/jpeg`, `image/gif`, `image/webp`) and `Ref` (the hash of the image bytes) to `msg.Part`.
   - The bytes are written once to `kc_message` with kind `image` (`Store.Put`). The message body holds only the hash.
   - The same image is stored once, however many times it is attached.
   - Old messages have no `image` parts, so reading them does not change. The new fields are `omitempty`.
2. **Size limits:** 5 MB per image, up to 8 images per message (matching the Anthropic API's limits). Anything over is refused with a reason before sending. Images are not shrunk (to avoid adding a dependency).
3. **Input paths**
   - **ACP (as an agent):** answer `promptCapabilities.image: true`. Accept `image` parts (base64 and `mimeType`) in `session/prompt`.
   - **API:** add `images: [{media_type, data(base64)}]` to `POST /sessions/{id}/messages`. `Runtime.Send` writes the text and image parts as one message.
   - **TUI:** pointing at an image file with `@<path>` (the same path as pointing at a file with `@`) attaches that file as an image. When a paste from the terminal puts in the path of an image file, it is handled the same way. Clipboard images themselves (not files) are not handled in this ADR.
4. **How each engine receives it**
   - **Anthropic API:** passed as an `image` block (base64).
   - **ACP engines (codex, copilot; ADR 0014):** if the other side has `promptCapabilities.image`, passed as an `image` part. If not, use "pass as a file" below.
   - **Claude Code engine (ADR 0007):** the current way of passing text with `-p` stays, and images use "pass as a file".
     - Claude Code can read images with `Read`.
     - Switching to `--input-format stream-json` would change how the engine is started, so it is decided separately.
   - **Pass as a file:** the image is written to `images/<hash>.<extension>` in the data directory (named by the content hash, so rewriting it gives the same thing). "Attached image: <path>" is appended to the end of the prompt.
     - The engine's sandbox is allowed to read that path (read only).
5. **Record and resume:** images are recorded by hash, so on resume the same image is passed as the same input (invariant 5). In `kairocode log` and in ACP's `session/load` replay, an image is shown as "[image: <kind>, <size>]" and the bytes are not output (ACP replay returns it as an `image` part).
6. **Secrets:** the content of an image is treated as something the user attached. Neither the bytes nor base64 go into logs or error messages.

Where this shows up in the implementation: `internal/msg` (the `Part` fields), `internal/store` (the `image` kind), `internal/runtime` (`Send`, how each engine receives images, the location and sandbox when passing as a file), `internal/provider` (Anthropic's `image` block), `internal/acp` (`promptCapabilities.image`, receiving and replaying `image` parts), `internal/server` and `internal/daemon` (`images`), `internal/tui` (images pointed at with `@`).

## Consequences

- The user can show an image with a request. Every engine gets either the image content or the image file.
- An image is stored once, and the input of a model call stays a list of hashes.
- Tests (added at implementation)
  - With a fake provider, an image part is recorded by hash and reaches the provider as an `image` block. The same after resume.
  - An `image` part in ACP's `session/prompt` is accepted as the schema says and is replayed by `session/load`.
  - An image over 5 MB, or of an unsupported kind, is refused before sending.
  - A fake Claude Code engine gets the image file's path in the prompt, and the file is in its location.
  - base64 does not appear in logs or error messages.

## Addendum (2026-10-27, decided during implementation)

- **The Claude Code engine passes images through `stream-json` input, not as files (the user's decision).** "Claude Code can read images with `Read`" in decision 4 was wrong. ADR 0007 turns off the built-in tools (`--tools ""`), so there is no `Read`. Bringing back only `Read` was considered, but rejected because it does not fit ADR 0007's reason (not causing effects outside KairoCode). Instead, only for turns with images, the prompt and images are passed on standard input with `--input-format stream-json` (ADR 0007's addendum). The alternatives rejected this as "changing how the engine is started", but in fact only the way input is passed changes, and output is read the same way. Checked with the real `claude` (2.1.311) that it can read images and can continue with `--resume`. Test: `TestClaudeEngineImages` (a fake `claude` receives the images on standard input).
- **"Pass as a file" is used only for ACP agents that do not read images** (those without `promptCapabilities.image`). The ACP agent reads them with its own tools. Test: `TestImagesAsFiles`.
- **Image bytes are stored in `kc_message` with kind `image`, and `msg.Part` holds only `MediaType` and `Ref`.** `Part.Data` is a field filled from `Ref` just before passing to the model, and is not stored (`json:"-"`). Images are also passed to the summary of history compaction (ADR 0020).
- **Limits (decision 2 adjusted to how the API measures):** the API's 5 MB per image is measured on the base64 length, so it is up to 3.75 MB per image in original bytes. The total of images in one message is also up to 15 MB in original bytes (20 MB in base64; an API request can be up to 32 MB). The check before sending (`runtime.CheckImages`) also looks at the kind, the 8-image limit, and whether the content is the stated kind (`http.DetectContentType`). Error messages do not include the content.
- **Images piling up in history:** one model call gets the newest images, up to 50 and up to 15 MB in total. Older images are passed as the text "an image attached earlier (kind); not sent again". Otherwise, in a session with many images, every following call would fail with "too large" and could never proceed (the record does not change, only how it is passed). Test: `TestImagesKeptToWhatACallTakes`.
- **Kill and resume:** if a turn with images on the Claude Code engine is killed midway and resumed, Claude Code already has the prompt, so only "continue" is sent and the images are not sent twice. Test: `TestCrashClaudeEngineImage`. In the error message when `claude` fails (the tail of stderr), long strings that look like base64 are masked.
- **Replay:** ACP's `session/load` returns images as `image` parts. The bytes are fetched from the daemon's `GET /images/{hash}` (only for the `image` kind). The TUI and `kairocode log` show `[image: <kind>, <size>]`.
- **TUI:** `@<path>` can point outside the working folder (absolute paths, `~/`) if it is an image file. This is meant for screenshots on the desktop. Non-image files are, as before, only inside the working folder. Image paths pasted by dropping a file on the terminal (quoted, or with spaces escaped as `\ `) are replaced with `@<path>`. Test: `TestMentionedImages`.
- Tests: `TestCheckImages`, `TestImagesReachTheModel` (the image reaches a fake provider, and also reaches it in the next turn; the stored message has no bytes; no base64 appears in the record), `TestAnthropicImages`, `TestACPImages`.

## Addendum (2026-10-28, Accepted (2026-10-28): pasting clipboard images)

The user wants to paste screenshots from the clipboard without saving them as files (the user's request, 2026-10-28). This is what decision 3's TUI part said was "not handled". A terminal sends only text on paste, so the TUI reads the clipboard itself.

- **Trigger:** Ctrl+V in the TUI's input box, and a paste with no content. With Cmd+V, when the clipboard holds only an image, the terminal sends nothing or sends an empty paste. In either case, if the clipboard has an image, it is attached. If there is no image, nothing happens (text paste works as before).
- **How it is read (no added dependency):**
  - macOS: `osascript` is made to write the clipboard's `«class PNGf»` to a file.
  - Linux: `wl-paste --type image/png` on Wayland, `xclip -selection clipboard -t image/png -o` on X11. If neither exists, the status line shows "reading clipboard images needs wl-paste or xclip".
  - It gives up after 5 seconds.
- **Location:** the PNG read is written to `pasted/<content hash>.png` in the data directory, and `@<path>` is put in the input box. From there it follows the same path as today's `@<path>` images (size and kind checks, storing in `kc_message`). Pasting the same image many times gives one file. The file is not needed after sending, but so that it does not disappear before sending, files older than 7 days are deleted by the TUI on the next paste (in the implementation this was put on the pasting side, not the daemon).
- **Starting commands outside the sandbox (ADR 0005):** the clipboard belongs to the user's desktop, so it cannot be read from inside KairoCode's sandbox. Only the fixed commands and arguments above are started, and no model output is passed to them. They run only on the user's key input. `internal/tui/clipboard.go` is added to the files allowed to start commands (`commandStarters` in `internal/invariants`).
- **Tests:** with the clipboard-reading function replaced, Ctrl+V and an empty paste put `@<path>` in; when there is no image or it cannot be read, nothing is put in and a reason is shown; the same image becomes one file. With a real clipboard, it is checked by hand (macOS and Linux).
- **How it looks in the input box (2026-10-28, changed after the user pointed it out):** the input box gets a label like `[Image #1]` instead of the path (the same as Claude Code). When sending, if the text has the label, that image is attached. Deleting the label means it is not attached. An image file dropped on the terminal also becomes a label. The message text is sent with the label as is.
- **Implementation (2026-10-28):** `internal/tui/clipboard.go`. If there is no image, Ctrl+V is passed to the input box and pastes text as before. In a TUI that does not know the data directory, Ctrl+V belongs to the input box. macOS's `osascript` was checked with a screenshot on the real clipboard (the PNG was read). Linux's `wl-paste` and `xclip` have not been checked by hand. Tests: `TestPasteImage`, `TestPasteNoImage`, `TestPasteWithNoDataDir`, `TestSavePasted`.
- **Alternatives considered:** terminal image protocols (kitty, iTerm2) are for the terminal to display images, and do not send pastes. Go clipboard libraries either use cgo or call the same commands, so there is no reason to add a dependency.

## Alternatives considered

- **Put images in the message body as base64:** storage is simple. But every read of the history reads the images, and the same image is stored many times. Hashing messages also becomes heavy.
- **Always turn images into files and pass only the path:** the smallest implementation. But the Anthropic API engine cannot see an image through a file-reading tool (`read` returns text). What can be passed to the API directly is passed directly.
- **Switch the Claude Code engine to `--input-format stream-json`:** image blocks can be passed as is. But how the engine is started and how its output is read would change, which means revisiting ADR 0007. Passing as a file gets there with the current form.
