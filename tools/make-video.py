#!/usr/bin/env python3
"""Joins the frames from web/e2e/record-demo.mjs with the voice track into an MP4.

Each output frame at 30 fps shows the newest captured frame at that instant, so the
picture cannot drift from the voice.

Usage: python tools/make-video.py framesDir voiceFile out.mp4
Needs: pip install imageio-ffmpeg
"""
import bisect
import json
import os
import subprocess
import sys

import imageio_ffmpeg

FPS = 30
frames_dir, voice, out = sys.argv[1:4]
meta = json.load(open(os.path.join(frames_dir, "frames.json")))
frames = sorted(meta["frames"], key=lambda f: f["ts"])
stamps = [f["ts"] for f in frames]
total = round((meta["end"] - meta["t0"]) * FPS)

ff = subprocess.Popen(
    [imageio_ffmpeg.get_ffmpeg_exe(), "-y", "-loglevel", "error", "-f", "image2pipe", "-c:v", "mjpeg", "-framerate", str(FPS), "-i", "-", "-i", voice,
     "-map", "0:v", "-map", "1:a", "-vf", "scale=1920:1080:flags=lanczos,format=yuv420p", "-c:v", "libx264", "-preset", "medium", "-crf", "20", "-r", str(FPS),
     "-af", "loudnorm=I=-16:TP=-1.5:LRA=11", "-ar", "48000", "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", out],
    stdin=subprocess.PIPE,
)
cached_name, cached = None, b""
for n in range(total):
    at = meta["t0"] + n / FPS
    i = max(bisect.bisect_right(stamps, at) - 1, 0)
    name = frames[i]["name"]
    if name != cached_name:
        with open(os.path.join(frames_dir, name), "rb") as f:
            cached_name, cached = name, f.read()
    ff.stdin.write(cached)
ff.stdin.close()
if ff.wait() != 0:
    sys.exit("ffmpeg failed")
print("wrote", out, f"{os.path.getsize(out) / 1e6:.1f} MB,", total, "frames")
