//go:build darwin

// Platform backend for notify.Send on darwin.
//
// Notifications are dispatched via UNUserNotificationCenter (the modern
// UserNotifications.framework). NSUserNotification was deprecated in
// macOS 11 (Big Sur, 2020) and became fully non-functional on macOS 26
// (Tahoe) — even from a properly code-signed bundle with granted
// permission, deliverNotification: silently dropped. Migrated 2026-06-18.
//
// Requirements that still apply:
//   - The binary must be running from inside a registered .app bundle
//     (so macOS resolves the bundle identity for permission scoping).
//   - The user must have granted notification permission for the bundle
//     ID at least once via System Settings → Notifications (or via the
//     authorization prompt that the requestAuthorization call below
//     triggers on first run).
//   - The title is left empty so macOS uses CFBundleDisplayName from the
//     bundle's Info.plist.
package notify

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework UserNotifications

#import <Foundation/Foundation.h>
#import <UserNotifications/UserNotifications.h>

static dispatch_once_t openkanbanAuthOnce;
static void openkanbanRequestAuth(void) {
    dispatch_once(&openkanbanAuthOnce, ^{
        UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
        [center requestAuthorizationWithOptions:UNAuthorizationOptionAlert
                              completionHandler:^(BOOL granted, NSError *error) {
            // intentionally unused — user grants via System Settings
        }];
    });
}

static void openkanbanSendNotification(const char *body) {
    @autoreleasepool {
        // UNUserNotificationCenter requires a bundle identity: from a
        // non-bundled process (a CLI/test binary, a $PATH or tui-fork
        // daemon) [UNUserNotificationCenter currentNotificationCenter]
        // raises an NSInternalInconsistencyException and abort()s the
        // whole process. Off-bundle we cannot deliver anyway, so no-op —
        // restoring the silent-off-bundle contract the NSUserNotification
        // backend had (and that notify_other.go documents). bundleIdentifier
        // is nil iff we're off-bundle; non-nil in the .app (dev.cmeid.openkanban).
        if ([[NSBundle mainBundle] bundleIdentifier] == nil) {
            return;
        }
        openkanbanRequestAuth();

        UNMutableNotificationContent *content = [[UNMutableNotificationContent alloc] init];
        content.body = [NSString stringWithUTF8String:body];

        UNNotificationRequest *request =
            [UNNotificationRequest requestWithIdentifier:[[NSUUID UUID] UUIDString]
                                                 content:content
                                                 trigger:nil];

        UNUserNotificationCenter *center = [UNUserNotificationCenter currentNotificationCenter];
        [center addNotificationRequest:request
                 withCompletionHandler:^(NSError *error) {
            // fire-and-forget
        }];
    }
}
*/
import "C"

import "unsafe"

func platformSend(body string) error {
	cBody := C.CString(body)
	defer C.free(unsafe.Pointer(cBody))
	C.openkanbanSendNotification(cBody)
	return nil
}
