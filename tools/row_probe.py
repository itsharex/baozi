from PIL import Image

p = r'C:\Users\Administrator\AppData\Local\DoubaoWork\User Data\ClipboardTemp\a75e0a69-98e4-4342-9141-9123a8836d1d.png'
im = Image.open(p).convert('RGB')
px = im.load()


def lum(c):
    return int(0.3 * c[0] + 0.6 * c[1] + 0.1 * c[2])


for y in (16, 20, 26, 30, 40):
    vals = []
    for x in range(0, 34):
        c = px[x, y]
        vals.append('%02d:%02X%02X%02X/%d' % (x, c[0], c[1], c[2], lum(c)))
    print('y=%2d' % y)
    print('  ' + ' '.join(vals[:17]))
    print('  ' + ' '.join(vals[17:]))
    print()
