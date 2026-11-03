import { ui } from './api.svelte.js';

const CHUNK = 8 << 20;
const SPEED_WINDOW = 4000; // Long enough to smooth out the pause between chunks.
const ESTIMATE_AFTER = 2000; // The first moments say little about how long the rest will take.

// One chunk over XMLHttpRequest, because fetch can't report upload progress.
function sendChunk(url, blob, onBytes) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api' + url);
    xhr.upload.onprogress = (e) => onBytes(e.loaded);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve();
        return;
      }
      if (xhr.status === 401) ui.authed = false;
      let message = `Request failed (${xhr.status})`;
      try {
        message = JSON.parse(xhr.responseText).error || message;
      } catch {
        // Not JSON, usually a proxy's error page; the status says enough.
      }
      reject(new Error(message));
    };
    xhr.onerror = () => reject(new Error('The connection failed during the upload'));
    xhr.send(blob);
  });
}

// Sends a file in pieces, so nginx's body limit, Cloudflare's request limit and flaky
// networks don't matter. onProgress receives the bytes sent so far.
export async function uploadChunks(file, dest, onProgress) {
  let offset = 0;
  do {
    const chunk = file.slice(offset, offset + CHUNK);
    const final = offset + chunk.size >= file.size ? '&final=1' : '';
    await sendChunk(`/files/upload?path=${encodeURIComponent(dest)}&offset=${offset}${final}`, chunk, (sent) => onProgress?.(offset + sent));
    offset += chunk.size;
    onProgress?.(offset);
  } while (offset < file.size);
}

// Measures a transfer on a timer rather than per progress event, so a stalled upload reads as
// a falling speed instead of showing its last value forever. onSpeed gets bytes per second,
// and seconds left when that's known.
export function trackSpeed(total, onSpeed) {
  const started = performance.now();
  const samples = [[started, 0]];
  let done = 0;
  const timer = setInterval(() => {
    const now = performance.now();
    samples.push([now, done]);
    while (samples.length > 2 && now - samples[0][0] > SPEED_WINDOW) samples.shift();
    const [from, bytes] = samples[0];
    const speed = (done - bytes) / ((now - from) / 1000);
    onSpeed(speed, speed > 0 && now - started > ESTIMATE_AFTER ? (total - done) / speed : null);
  }, 500);
  return {
    set(bytes) {
      done = bytes;
    },
    // Ends the timer and returns the average speed of the whole transfer.
    stop() {
      clearInterval(timer);
      return done / Math.max((performance.now() - started) / 1000, 0.001);
    },
  };
}

// Uploads to a temporary path the server removes once the import has run.
export async function uploadForImport(file, onProgress) {
  const random = Array.from(crypto.getRandomValues(new Uint8Array(8)), (b) => b.toString(16).padStart(2, '0')).join('');
  const dest = `/tmp/hypanel-import-${random}`;
  await uploadChunks(file, dest, onProgress);
  return dest;
}
