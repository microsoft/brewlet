"""Render the shared social preview with bundled fonts and no network access."""

from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

WIDTH, HEIGHT = 1200, 630
SCALE = 2
BACKGROUND = "#fffdf9"
INK = "#2a1f18"
COFFEE = "#6f4e37"
BLUE = "#326ce5"


def render(destination):
    image = Image.new("RGB", (WIDTH * SCALE, HEIGHT * SCALE), BACKGROUND)
    draw = ImageDraw.Draw(image)

    def rectangle(box, fill, radius=0):
        draw.rounded_rectangle(tuple(value * SCALE for value in box),
                               radius=radius * SCALE, fill=fill)

    def text(position, value, size, fill=INK):
        draw.text(tuple(value * SCALE for value in position), value,
                  font=ImageFont.load_default(size=size * SCALE), fill=fill)

    rectangle((0, 0, WIDTH, 10), BLUE)
    # Match the coffee-bean mark used by the landing page.
    bean = Image.new("RGBA", (64 * SCALE, 78 * SCALE))
    mark = ImageDraw.Draw(bean)
    mark.ellipse((9 * SCALE, 5 * SCALE, 55 * SCALE, 73 * SCALE), fill=COFFEE)
    mark.line([(x * SCALE, y * SCALE) for x, y in
               [(35, 7), (27, 21), (26, 30), (36, 47), (36, 56), (28, 72)]],
              fill=BACKGROUND, width=3 * SCALE, joint="curve")
    bean = bean.rotate(-24, resample=Image.Resampling.BICUBIC)
    image.paste(bean, (52 * SCALE, 44 * SCALE), bean)
    text((128, 57), "brewlet", 46, COFFEE)
    text((60, 192), "Ship the app.", 76)
    text((60, 282), "Not the runtime.", 76, COFFEE)
    text((64, 405), "Your application is the workload.", 27)
    text((64, 448), "The runtime is infrastructure.", 27, COFFEE)

    rectangle((820, 174, 1140, 274), "#f5e6c8", 14)
    text((842, 195), "Your application", 29)
    text((842, 237), "Code + dependencies", 19, COFFEE)
    text((965, 284), "+", 42, BLUE)
    rectangle((820, 350, 1140, 450), BLUE, 14)
    text((842, 371), "Approved runtime", 28, "#ffffff")
    text((842, 413), "Supplied by infrastructure", 19, "#ffffff")
    text((844, 481), "Composed on Kubernetes", 20, COFFEE)

    rectangle((0, 553, WIDTH, HEIGHT), "#fbf4e6")
    text((1000, 578), "brewlet.sh", 23, COFFEE)
    destination = Path(destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    image.resize((WIDTH, HEIGHT), Image.Resampling.LANCZOS).save(
        destination, format="PNG", optimize=True)


if __name__ == "__main__":
    render(Path(__file__).resolve().parents[1] / "_site/assets/images/social-card.png")
