// Clipboard access with a fallback for non-secure contexts.
//
// `navigator.clipboard` is gated on a SECURE CONTEXT: it is undefined on a plain-http origin unless
// that origin is localhost. The portal's nginx serves http on :8080 with no TLS (see the "platform
// HA/TLS" shortcut in CLAUDE.md), so the modern API works when you browse the deployment from the
// host itself and silently disappears the moment anyone reaches it by IP or hostname - which is the
// normal way a deployed platform is used. Every copy button would be a no-op there.
//
// So: try the async API, fall back to the legacy `document.execCommand('copy')` over an off-screen
// textarea (which has no secure-context requirement), and report failure to the caller rather than
// swallowing it - a copy button that quietly does nothing is worse than one that says it couldn't.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Permission denied, or the document wasn't focused. Fall through to the legacy path.
    }
  }
  return legacyCopy(text);
}

// legacyCopy selects the text in an off-screen textarea and asks the document to copy the selection.
// Deprecated, but it is the only path available on an insecure origin and is still implemented by
// every browser we care about. It must run inside the user-gesture task that triggered the copy.
function legacyCopy(text: string): boolean {
  const ta = document.createElement('textarea');
  ta.value = text;
  // Keep it out of view and out of the layout, but still focusable/selectable - `display: none` or
  // `visibility: hidden` would make the selection (and so the copy) fail.
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.top = '-9999px';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  try {
    ta.select();
    ta.setSelectionRange(0, text.length);
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    document.body.removeChild(ta);
  }
}
