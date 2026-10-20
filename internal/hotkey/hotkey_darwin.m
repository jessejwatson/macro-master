// Global hotkeys through Carbon's RegisterEventHotKey, which needs no
// Accessibility or Input Monitoring permission. Hotkey events arrive on
// the main thread's event loop, so registering happens there too.

#import <Cocoa/Cocoa.h>
#import <Carbon/Carbon.h>

extern void mmHotkeyPressed(unsigned int id);

static OSStatus mm_on_hotkey(EventHandlerCallRef next, EventRef event, void *data) {
	EventHotKeyID hk;
	if (GetEventParameter(event, kEventParamDirectObject, typeEventHotKeyID,
			NULL, sizeof(hk), NULL, &hk) == noErr && hk.signature == 'mmhk') {
		mmHotkeyPressed(hk.id);
	}
	return noErr;
}

// mm_run runs the event loop on the main thread, as a background app with
// no Dock icon or menu bar. It never returns.
void mm_run(void) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyProhibited];
		EventTypeSpec spec = { kEventClassKeyboard, kEventHotKeyPressed };
		InstallApplicationEventHandler(&mm_on_hotkey, 1, &spec, NULL, NULL);
		[NSApp run];
	}
}

// mm_register registers a hotkey from a thread other than the main one,
// waiting until the event loop has done it.
int mm_register(unsigned int id, unsigned int code, unsigned int mods, void **ref) {
	__block OSStatus status;
	__block EventHotKeyRef out = NULL;
	dispatch_sync(dispatch_get_main_queue(), ^{
		EventHotKeyID hk = { 'mmhk', id };
		status = RegisterEventHotKey(code, mods, hk, GetApplicationEventTarget(), 0, &out);
	});
	*ref = out;
	return status;
}

void mm_unregister(void *ref) {
	dispatch_sync(dispatch_get_main_queue(), ^{
		UnregisterEventHotKey((EventHotKeyRef)ref);
	});
}
