//go:build darwin

// Platform backend for notify.Send on darwin.
//
// The notification is dispatched via NSUserNotification. The title is
// left empty so macOS substitutes the bundle's CFBundleDisplayName from
// the .app the binary runs from; this requires the binary be launched
// from a registered .app bundle to surface a usable name.
package notify

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation

#import <Foundation/Foundation.h>

static void openkanbanSendNotification(const char *body) {
    @autoreleasepool {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
        NSUserNotification *n = [[NSUserNotification alloc] init];
        n.informativeText = [NSString stringWithUTF8String:body];
        [[NSUserNotificationCenter defaultUserNotificationCenter] deliverNotification:n];
#pragma clang diagnostic pop
    }
}
*/
import "C"

import "unsafe"

// platformSend is the darwin backend invoked by notify.Send's default.
//
// The notification title is left empty so macOS uses CFBundleDisplayName
// from the .app bundle the binary runs from. The body is passed through
// verbatim as the notification's informativeText.
//
// Returns nil; NSUserNotificationCenter's deliverNotification: does not
// expose a meaningful synchronous error.
func platformSend(body string) error {
	cBody := C.CString(body)
	defer C.free(unsafe.Pointer(cBody))
	C.openkanbanSendNotification(cBody)
	return nil
}
