//go:build darwin && cgo

package capture

// macOS capture without kbinani/screenshot: its #cgo line links
// ScreenCaptureKit unconditionally (cgo ignores the surrounding #if), so the
// binary would not even load before macOS 12.3. Here nothing references
// ScreenCaptureKit or the CoreGraphics capture calls at link time:
//   - macOS 14+: ScreenCaptureKit, dlopen'ed and reached through
//     NSClassFromString (CGDisplayCreateImage is obsoleted in macOS 15).
//   - older:     CGDisplayCreateImage, looked up with dlsym (so it also
//     compiles with a deployment target where it's marked unavailable).

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation -framework Foundation
#include <dlfcn.h>
#include <CoreGraphics/CoreGraphics.h>
#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>

typedef CGImageRef (*cgDisplayCreateImageFn)(CGDirectDisplayID);

static CGImageRef captureSCK(CGDirectDisplayID id, size_t w, size_t h) {
	static dispatch_once_t once;
	dispatch_once(&once, ^{
		dlopen("/System/Library/Frameworks/ScreenCaptureKit.framework/ScreenCaptureKit", RTLD_LAZY);
	});
	Class contentClass = NSClassFromString(@"SCShareableContent");
	Class filterClass = NSClassFromString(@"SCContentFilter");
	Class configClass = NSClassFromString(@"SCStreamConfiguration");
	Class managerClass = NSClassFromString(@"SCScreenshotManager");
	if (!contentClass || !filterClass || !configClass || !managerClass) return NULL;

	dispatch_semaphore_t sem = dispatch_semaphore_create(0);
	__block CGImageRef result = NULL;
	if (@available(macOS 14.0, *)) {
		[contentClass getShareableContentWithCompletionHandler:^(SCShareableContent *content, NSError *error) {
			@autoreleasepool {
				SCDisplay *target = nil;
				if (!error) {
					for (SCDisplay *d in content.displays) {
						if (d.displayID == id) { target = d; break; }
					}
				}
				if (!target) { dispatch_semaphore_signal(sem); return; }
				SCContentFilter *filter = [[filterClass alloc] initWithDisplay:target excludingWindows:@[]];
				SCStreamConfiguration *config = [[configClass alloc] init];
				config.sourceRect = CGRectMake(0, 0, w, h);
				config.width = w;
				config.height = h;
				config.showsCursor = NO;
				[managerClass captureImageWithFilter:filter configuration:config
					completionHandler:^(CGImageRef img, NSError *err) {
						if (!err && img) result = CGImageRetain(img);
						dispatch_semaphore_signal(sem);
					}];
				[filter release];
				[config release];
			}
		}];
	} else {
		dispatch_semaphore_signal(sem);
	}
	dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
	dispatch_release(sem);
	return result;
}

static CGImageRef captureCG(CGDirectDisplayID id) {
	static cgDisplayCreateImageFn fn;
	static dispatch_once_t once;
	dispatch_once(&once, ^{
		fn = (cgDisplayCreateImageFn)dlsym(RTLD_DEFAULT, "CGDisplayCreateImage");
	});
	return fn ? fn(id) : NULL;
}

// captureDisplay draws display id into buf (w*h, 4 bytes per pixel, RGBX).
static int captureDisplay(CGDirectDisplayID id, void *buf, size_t w, size_t h, size_t stride) {
	CGImageRef img = NULL;
	if (@available(macOS 14.0, *)) {
		img = captureSCK(id, w, h);
	} else {
		img = captureCG(id);
	}
	if (!img) return -1;

	CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
	// Relabel as sRGB (no conversion) so drawing below is a plain copy/scale.
	CGImageRef srgb = CGImageCreateCopyWithColorSpace(img, cs);
	CGImageRelease(img);
	if (!srgb) { CGColorSpaceRelease(cs); return -2; }

	CGContextRef ctx = CGBitmapContextCreate(buf, w, h, 8, stride, cs,
		(CGBitmapInfo)kCGImageAlphaNoneSkipLast | (CGBitmapInfo)kCGBitmapByteOrder32Big);
	CGColorSpaceRelease(cs);
	if (!ctx) { CGImageRelease(srgb); return -3; }
	CGContextDrawImage(ctx, CGRectMake(0, 0, w, h), srgb);
	CGContextRelease(ctx);
	CGImageRelease(srgb);
	return 0;
}
*/
import "C"

import (
	"errors"
	"image"
	"unsafe"
)

// Screen is the default Capturer.
type Screen struct{}

// displayIDs lists active displays, main display first (index 0), then the
// rest in system order, matching the other platforms' numbering.
func displayIDs() []C.CGDirectDisplayID {
	var n C.uint32_t
	if C.CGGetActiveDisplayList(0, nil, &n) != C.kCGErrorSuccess || n == 0 {
		return nil
	}
	ids := make([]C.CGDirectDisplayID, n)
	if C.CGGetActiveDisplayList(n, &ids[0], &n) != C.kCGErrorSuccess {
		return nil
	}
	main := C.CGMainDisplayID()
	out := []C.CGDirectDisplayID{main}
	for _, id := range ids[:n] {
		if id != main {
			out = append(out, id)
		}
	}
	return out
}

// bounds is the display's rectangle in global coordinates (points, origin at
// the main display's top-left, y down).
func bounds(id C.CGDirectDisplayID) image.Rectangle {
	r := C.CGDisplayBounds(id)
	x, y := int(r.origin.x), int(r.origin.y)
	return image.Rect(x, y, x+int(r.size.width), y+int(r.size.height))
}

func (Screen) Displays() []Display {
	ids := displayIDs()
	out := make([]Display, 0, len(ids))
	for i, id := range ids {
		b := bounds(id)
		out = append(out, Display{Index: i, Bounds: b, Width: b.Dx(), Height: b.Dy()})
	}
	return out
}

func (Screen) Capture(display int) (*image.RGBA, error) {
	ids := displayIDs()
	if display < 0 || display >= len(ids) {
		return nil, ErrNoDisplay
	}
	id := ids[display]
	b := bounds(id)
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, ErrNoDisplay
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	buf := C.malloc(C.size_t(len(img.Pix)))
	if buf == nil {
		return nil, errors.New("capture: out of memory")
	}
	defer C.free(buf)
	if C.captureDisplay(id, buf, C.size_t(w), C.size_t(h), C.size_t(img.Stride)) != 0 {
		return nil, errors.New("capture: cannot capture display (Screen Recording permission?)")
	}
	copy(img.Pix, unsafe.Slice((*byte)(buf), len(img.Pix)))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	return img, nil
}
