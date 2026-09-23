"""Regenerate render/testdata/qwen-image-21-sigmas.golden.json.

The golden arrays behind render/wf-qwen-image-21.test.mjs come from the REAL
diffusers scheduler, not from a re-implementation: FlowMatchEulerDiscreteScheduler
built from Qwen/Qwen-Image-2.1's scheduler/scheduler_config.json (copied verbatim
below), driven exactly the way QwenImage21Pipeline drives it:

    sigmas = np.linspace(1.0, 1 / steps, steps)
    mu = calculate_shift(tokens, base_image_seq_len, max_image_seq_len, base_shift, max_shift)
    scheduler.set_timesteps(sigmas=sigmas, mu=mu)

with tokens = (H // 16) * (W // 16) (the pipeline's packed target latent length;
vae_scale_factor 16). Needs diffusers >= 0.40.0 (exponential time shift and
shift_terminal). Not run in CI: CI compares the committed fixture with the JS.

    python render/testdata/gen-qwen-image-21-sigmas.py
"""

import json
import os

import diffusers
import numpy as np
from diffusers import FlowMatchEulerDiscreteScheduler

# Qwen/Qwen-Image-2.1 scheduler/scheduler_config.json
SCHEDULER_CONFIG = {
    "_class_name": "FlowMatchEulerDiscreteScheduler",
    "base_image_seq_len": 256,
    "base_shift": 0.5,
    "invert_sigmas": False,
    "max_image_seq_len": 8192,
    "max_shift": 0.9,
    "num_train_timesteps": 1000,
    "shift": 1.0,
    "shift_terminal": 0.02,
    "stochastic_sampling": False,
    "time_shift_type": "exponential",
    "use_beta_sigmas": False,
    "use_dynamic_shifting": True,
    "use_exponential_sigmas": False,
    "use_karras_sigmas": False,
}

CASES = [(2048, 2048, 40), (1024, 1024, 40), (2752, 1536, 40), (1024, 1024, 25)]


def calculate_shift(image_seq_len, base_seq_len, max_seq_len, base_shift, max_shift):
    # diffusers.pipelines.qwenimage21.pipeline_qwenimage21.calculate_shift
    m = (max_shift - base_shift) / (max_seq_len - base_seq_len)
    b = base_shift - m * base_seq_len
    return image_seq_len * m + b


def main():
    out = {
        "generator": "diffusers %s FlowMatchEulerDiscreteScheduler + QwenImage21Pipeline linspace/calculate_shift"
        % diffusers.__version__,
        "scheduler_config": SCHEDULER_CONFIG,
        "cases": [],
    }
    for width, height, steps in CASES:
        s = FlowMatchEulerDiscreteScheduler.from_config(SCHEDULER_CONFIG)
        tokens = (height // 16) * (width // 16)
        mu = calculate_shift(
            tokens,
            s.config.get("base_image_seq_len", 256),
            s.config.get("max_image_seq_len", 4096),
            s.config.get("base_shift", 0.5),
            s.config.get("max_shift", 1.15),
        )
        s.set_timesteps(sigmas=np.linspace(1.0, 1 / steps, steps), mu=mu, device="cpu")
        out["cases"].append(
            {
                "width": width,
                "height": height,
                "steps": steps,
                "tokens": tokens,
                "mu": mu,
                "sigmas": [float(x) for x in s.sigmas.tolist()],
            }
        )
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "qwen-image-21-sigmas.golden.json")
    with open(path, "w", newline="\n") as f:
        json.dump(out, f, indent=1)
        f.write("\n")
    print("wrote", path)


if __name__ == "__main__":
    main()
