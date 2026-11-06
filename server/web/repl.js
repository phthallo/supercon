// MicroPython raw REPL over a pair of web streams (a Web Serial port in the browser).
class Repl {
  constructor(readable, writable) {
    this.reader = readable.getReader();
    this.writer = writable.getWriter();
    this.buf = "";
    this.decoder = new TextDecoder();
    this.waiters = [];
    this.closed = false;
    this.pump();
  }

  async pump() {
    try {
      for (;;) {
        const { value, done } = await this.reader.read();
        if (done) break;
        this.buf += this.decoder.decode(value, { stream: true });
        this.waiters.forEach((w) => w());
      }
    } catch (e) {
      // The port closed or the badge reset.
    }
    this.closed = true;
    this.waiters.forEach((w) => w());
  }

  // Resolves with everything read up to and including `marker`, and drops it from the buffer.
  until(marker, ms = 5000) {
    return new Promise((resolve, reject) => {
      const check = () => {
        const i = this.buf.indexOf(marker);
        if (i >= 0) {
          done();
          const out = this.buf.slice(0, i + marker.length);
          this.buf = this.buf.slice(i + marker.length);
          resolve(out);
        } else if (this.closed) {
          done();
          reject(new Error("The badge disconnected."));
        }
      };
      const timer = setTimeout(() => { done(); reject(new Error("The badge stopped responding.")); }, ms);
      const done = () => { clearTimeout(timer); this.waiters = this.waiters.filter((w) => w !== check); };
      this.waiters.push(check);
      check();
    });
  }

  async write(text) {
    const bytes = new TextEncoder().encode(text);
    // Small slices with pauses, so the badge's USB receive buffer doesn't overflow.
    for (let i = 0; i < bytes.length; i += 256) {
      await this.writer.write(bytes.slice(i, i + 256));
      await new Promise((r) => setTimeout(r, 10));
    }
  }

  async enter() {
    // Ctrl-C stops the running app; Ctrl-A switches to raw mode.
    await this.write("\r\x03\x03");
    await new Promise((r) => setTimeout(r, 300));
    this.buf = "";
    await this.write("\x01");
    await this.until("raw REPL; CTRL-B to exit\r\n>");
  }

  // Runs code and returns its stdout. Throws with the traceback if it raised.
  async exec(code, ms = 10000) {
    await this.write(code);
    await this.write("\x04");
    await this.until("OK", ms);
    const out = await this.until("\x04", ms);
    const err = await this.until("\x04>", ms);
    if (err.length > 2) throw new Error(err.slice(0, -2).trim());
    return out.slice(0, -1);
  }

  async writeFile(path, bytes) {
    await this.exec(`import binascii\n_f=open(${JSON.stringify(path)},'wb')`);
    try {
      for (let i = 0; i < bytes.length; i += 768) {
        let bin = "";
        for (const b of bytes.slice(i, i + 768)) bin += String.fromCharCode(b);
        await this.exec(`_f.write(binascii.a2b_base64('${btoa(bin)}'))`);
      }
    } finally {
      await this.exec("_f.close()");
    }
  }

  // The connection drops as the badge restarts, so nothing is read back.
  async reset() {
    await this.write("import machine\nmachine.reset()\x04");
  }

  async close() {
    try { await this.reader.cancel(); } catch (e) {}
    this.reader.releaseLock();
    this.writer.releaseLock();
  }
}
