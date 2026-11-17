from PIL import Image

p = r'C:\Users\Administrator\AppData\Local\DoubaoWork\User Data\ClipboardTemp\a75e0a69-98e4-4342-9141-9123a8836d1d.png'
im = Image.open(p).convert('RGB')
w, h = im.size
px = im.load()
print('size', w, h)

for y in range(h):
    xs = [x for x in range(w) if px[x, y][0] > 190]
    left = min(xs) if xs else None
    head = ''.join('%02X%02X%02X ' % px[x, y] for x in range(0, min(w, 10)))
    print('%2d edgeX=%-4s %s' % (y, left, head))
