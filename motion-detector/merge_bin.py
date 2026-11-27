# PlatformIO post-build step: merge bootloader, partition table and app into one
# image that flashes at offset 0 (what the web flasher writes).
Import("env")
import os

def merge(source, target, env):
    build = env.subst("$BUILD_DIR")
    out = os.path.join(build, "motion-detector-%s.bin" % env.subst("$PIOENV"))
    parts = []
    for addr, path in env.get("FLASH_EXTRA_IMAGES", []):
        parts += [addr, env.subst(path)]
    parts += [env.subst("$ESP32_APP_OFFSET"), os.path.join(build, env.subst("${PROGNAME}.bin"))]
    cmd = [env.subst("$PYTHONEXE"), env.subst("$OBJCOPY"), "--chip", "esp32", "merge_bin",
           "-o", out, "--flash_mode", "dio", "--flash_size", "4MB"] + parts
    print("Merging into", out)
    env.Execute(" ".join('"%s"' % c for c in cmd))

env.AddPostAction("$BUILD_DIR/${PROGNAME}.bin", merge)
