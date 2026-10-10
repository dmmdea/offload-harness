#!/usr/bin/env python
# atomic_out.py — how a Python render worker delivers a FINAL output file. Standard library only, so
# it imports in every worker venv (PIL for edit_image.py, torch for tts_chatterbox.py) and runs on its own.
#
# The same rule as render/atomic-out.mjs, for the workers that are Python. A worker that saves straight
# onto its output path opens (so truncates) the target before it writes a byte: a full disk leaves a
# zero-byte file that every exists() check reads as finished, and a previous GOOD file at that path is
# destroyed by the failed save (2026-10-09, an overnight picture batch). So the worker saves to a hidden
# sibling in the SAME directory and only a save that finished is renamed over the target (os.replace is
# atomic on POSIX and replaces an existing file on Windows). Any failure removes the sibling and re-raises
# the same exception: the target is never half written and a good file already there is kept.
#
# Run as a script, `--selftest` exercises it (no deps, no GPU): the way edit_image.py's selftest does.
import itertools
import os
import sys
import time

_seq = itertools.count(1)

# A rename onto a file can fail for a moment on Windows while an antivirus scanner holds the staged
# file; a few short retries, then the real error (render/atomic-out.mjs does the same).
_REPLACE_BACKOFF_S = (0.05, 0.1, 0.2, 0.4)


def partial_sibling(out):
    """Where a result is staged: beside `out` (same volume, so the rename is atomic), hidden, unique per
    process and call, and with the output's extension LAST because PIL and torchaudio pick the format from it."""
    folder, name = os.path.split(out)
    stem, ext = os.path.splitext(name)
    return os.path.join(folder, ".%s.partial-%d-%d%s" % (stem, os.getpid(), next(_seq), ext))


def _discard(partial):
    try:
        os.unlink(partial)
    except OSError:
        pass  # best effort: the error being reported matters more than the litter


def save_atomic(out, save, replace=os.replace, sleep=time.sleep):
    """Deliver `out` through `save(path)`, which must write the complete file at `path`. An empty result is
    refused (a 0-byte file looks finished). `replace` and `sleep` are injectable for the selftest only."""
    partial = partial_sibling(out)
    try:
        save(partial)
        if os.path.getsize(partial) == 0:
            raise OSError("refusing to deliver a 0-byte output at %s: an empty file looks finished to anything that checks it exists" % out)
        for pause in _REPLACE_BACKOFF_S:
            try:
                replace(partial, out)
                return
            except PermissionError:
                sleep(pause)
        replace(partial, out)
    except BaseException as e:
        _discard(partial)
        # name the OUTPUT in the message, not the staged sibling (or nothing, for a write that fails after the open)
        if isinstance(e, OSError) and e.filename in (None, partial):
            e.filename = out
        raise


def selftest():
    import errno
    import tempfile

    def names(d):
        return sorted(os.listdir(d))

    def raises(fn, kind):
        try:
            fn()
        except kind as e:
            return e
        raise AssertionError("expected %s" % kind.__name__)

    good = b"the previous good render"
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.png")

        # 1. delivery: the bytes land at out, nothing is left beside it
        save_atomic(out, lambda p: open(p, "wb").write(b"PNGDATA"))
        assert open(out, "rb").read() == b"PNGDATA" and names(d) == ["a.png"], names(d)

        # 2. a save that fails the way a full disk does (the file exists, a few bytes landed, then the
        #    write raises ENOSPC) keeps the previous good file byte for byte and leaves nothing behind
        def full_disk(p):
            with open(p, "wb") as f:
                f.write(b"PNG")
            raise OSError(errno.ENOSPC, "No space left on device")
        e = raises(lambda: save_atomic(out, full_disk), OSError)
        assert e.errno == errno.ENOSPC and e.filename == out, (e.errno, e.filename)
        assert open(out, "rb").read() == b"PNGDATA" and names(d) == ["a.png"], names(d)

        # 3. ... and with nothing there before, nothing is there after
        gone = os.path.join(d, "b.png")
        raises(lambda: save_atomic(gone, full_disk), OSError)
        assert names(d) == ["a.png"], names(d)

        # 4. an empty result is refused, whatever was there stays
        raises(lambda: save_atomic(out, lambda p: open(p, "wb").close()), OSError)
        assert open(out, "rb").read() == b"PNGDATA" and names(d) == ["a.png"], names(d)

        # 5. a replace that is refused for a moment is retried and then delivers
        calls = []
        def flaky(src, dst):
            calls.append(src)
            if len(calls) <= 2:
                raise PermissionError(13, "Permission denied")
            os.replace(src, dst)
        waits = []
        save_atomic(out, lambda p: open(p, "wb").write(b"NEW"), replace=flaky, sleep=waits.append)
        assert open(out, "rb").read() == b"NEW" and len(calls) == 3 and waits == [0.05, 0.1] and names(d) == ["a.png"], (calls, waits, names(d))

        # 6. one that stays refused gives up after the bounded retries, removes the staged file, keeps the file
        calls[:] = []
        def always(src, dst):
            calls.append(src)
            raise PermissionError(13, "Permission denied")
        waits[:] = []
        raises(lambda: save_atomic(out, lambda p: open(p, "wb").write(b"NEWER"), replace=always, sleep=waits.append), PermissionError)
        assert len(calls) == 5 and waits == [0.05, 0.1, 0.2, 0.4], (calls, waits)
        assert open(out, "rb").read() == b"NEW" and names(d) == ["a.png"], names(d)

        # 7. the staged name: hidden, beside out, extension last, distinct per call
        a, b = partial_sibling(out), partial_sibling(out)
        assert a != b and os.path.dirname(a) == d and a.endswith(".png") and os.path.basename(a).startswith("."), (a, b)
    print("SELFTEST PASS")
    return 0


if __name__ == "__main__":
    if "--selftest" in sys.argv:
        sys.exit(selftest())
    print("usage: atomic_out.py --selftest  (a library for the render workers; see the header)", file=sys.stderr)
    sys.exit(2)
