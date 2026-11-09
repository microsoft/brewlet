"""Regenerate the printable field guide and its preview with local Chromium."""

import argparse
from pathlib import Path
import shutil
import subprocess
import tempfile

from PIL import Image

ROOT = Path(__file__).resolve().parents[2]
ASSETS = ROOT / "docs/assets/cheat-sheet"
SOURCE = ASSETS / "print.html"
PDF = ASSETS / "brewlet-cheat-sheet.pdf"
PREVIEW = ASSETS / "preview.png"


def find_browser():
    for name in ("chromium", "chromium-browser", "google-chrome", "google-chrome-stable"):
        browser = shutil.which(name)
        if browser:
            return browser
    chrome = Path("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
    if chrome.is_file():
        return str(chrome)
    return None


def render(browser):
    with tempfile.TemporaryDirectory(prefix="brewlet-print-") as directory:
        temporary = Path(directory)
        flags = [
            browser, "--headless", "--no-first-run", "--no-default-browser-check",
            "--disable-background-networking", "--disable-component-update",
            "--disable-extensions", "--hide-scrollbars", "--log-level=3",
            f"--user-data-dir={temporary / 'profile'}",
        ]
        subprocess.run(flags + [
            "--no-pdf-header-footer", f"--print-to-pdf={PDF}", SOURCE.as_uri(),
        ], check=True)
        screenshot = temporary / "preview.png"
        subprocess.run(flags + [
            "--window-size=1600,1400", "--force-device-scale-factor=1",
            f"--screenshot={screenshot}", SOURCE.as_uri(),
        ], check=True)
        with Image.open(screenshot) as image:
            # Physical A3 dimensions at Chromium's 96 CSS pixels per inch.
            width, height = round(420 * 96 / 25.4), round(297 * 96 / 25.4)
            if image.width < width or image.height < height:
                raise ValueError("Browser screenshot does not contain the full A3 sheet")
            preview = image.crop((0, 0, width, height))
            preview.thumbnail((1200, 1200), Image.Resampling.LANCZOS)
            preview.save(PREVIEW, optimize=True)
    print(f"Generated {PDF.relative_to(ROOT)} and {PREVIEW.relative_to(ROOT)}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--browser", help="Path to a local Chromium or Google Chrome executable")
    args = parser.parse_args()
    browser = args.browser or find_browser()
    if not browser:
        parser.error("Chromium or Google Chrome is required; pass its path with --browser")
    render(browser)
