// ClaudeBar — macOS Touch Bar monitor for Claude Code
// Shows: current model, today's token usage, DeepSeek balance

#import <Cocoa/Cocoa.h>
#import <CoreServices/CoreServices.h>
#import <CoreGraphics/CoreGraphics.h>
#import <dlfcn.h>
#import <objc/message.h>

// ============================================================
// DFRFoundation private API (pins Touch Bar to Control Strip)
// ============================================================
static void (*DFRSetPresence)(CFStringRef, Boolean) = NULL;
static void (*DFRSetCloseBox)(Boolean) = NULL;

static void LoadDFRFoundation(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        void *h = dlopen(
            "/System/Library/PrivateFrameworks/DFRFoundation.framework/DFRFoundation",
            RTLD_NOW | RTLD_LOCAL);
        if (h) {
            DFRSetPresence = dlsym(h, "DFRElementSetControlStripPresenceForIdentifier");
            DFRSetCloseBox = dlsym(h, "DFRSystemModalShowsCloseBoxWhenFrontMost");
        }
    });
}

// ============================================================
// DeepSeek V4 pricing (¥ / 1M tokens) — https://api-docs.deepseek.com/zh-cn/quick_start/pricing
// Flash: input ¥1, output ¥2  |  Pro: input ¥3, output ¥6
// ============================================================
static const double kPriceFlashInput    = 1.0;
static const double kPriceFlashOutput   = 2.0;
static const double kPriceFlashCacheHit = 0.02;  // cache_read_input_tokens
static const double kPriceProInput      = 3.0;
static const double kPriceProOutput     = 6.0;
static const double kPriceProCacheHit   = 0.025;

// ============================================================
// DataFetcher — reads Claude Code config & session files,
//              calls DeepSeek balance API
// ============================================================

typedef struct {
    NSInteger input;
    NSInteger output;
    NSInteger cacheRead;
    NSInteger cacheCreate;
} TokenUsage;

@interface DataFetcher : NSObject
- (NSString *)fetchModel;
- (NSString *)fetchAPIKey;
- (TokenUsage)fetchTodayUsage;
/// Per-model usage: modelName → @{@"input":NSNumber, @"output":NSNumber, @"cacheRead":NSNumber, @"cacheCreate":NSNumber}
- (NSDictionary *)fetchTodayUsageByModel;
/// Cost summed across all models using each model's pricing
- (double)computeTotalCostWithModelUsage:(NSDictionary *)modelUsage;
- (void)fetchBalanceWithForce:(BOOL)force completion:(void(^)(double))completion;
@end

@implementation DataFetcher {
    NSString *_settingsPath;
    NSString *_projectsPath;
    NSDateFormatter *_df;
    double _cachedBalance;
    NSDate *_lastBalanceFetch;
}

- (instancetype)init {
    if (self = [super init]) {
        _settingsPath = [@"~/.claude/settings.json" stringByExpandingTildeInPath];
        _projectsPath = [@"~/.claude/projects" stringByExpandingTildeInPath];
        _df = [[NSDateFormatter alloc] init];
        _df.dateFormat = @"yyyy-MM-dd";
        _cachedBalance = 0;
        _lastBalanceFetch = [NSDate distantPast];
    }
    return self;
}

- (NSDictionary *)readSettings {
    NSData *d = [NSData dataWithContentsOfFile:_settingsPath];
    if (!d) return nil;
    id obj = [NSJSONSerialization JSONObjectWithData:d options:0 error:nil];
    return [obj isKindOfClass:NSDictionary.class] ? obj : nil;
}

- (NSString *)fetchModel {
    NSDictionary *settings = [self readSettings];
    if (!settings) return @"Unknown";

    NSDictionary *env = settings[@"env"];
    if (![env isKindOfClass:NSDictionary.class]) env = nil;

    // 1. model alias ("sonnet"/"opus"/"haiku") -> resolve via ANTHROPIC_DEFAULT_*_MODEL
    NSString *alias = settings[@"model"];
    if ([alias isKindOfClass:NSString.class] && alias.length > 0) {
        NSString *upper = alias.uppercaseString;
        NSString *key = [NSString stringWithFormat:@"ANTHROPIC_DEFAULT_%@_MODEL", upper];
        NSString *resolved = env[key];
        if ([resolved isKindOfClass:NSString.class] && resolved.length > 0) {
            return [self cleanModelName:resolved];
        }
    }

    // 2. ANTHROPIC_MODEL env override (direct model name)
    NSString *direct = env[@"ANTHROPIC_MODEL"];
    if ([direct isKindOfClass:NSString.class] && direct.length > 0) {
        return [self cleanModelName:direct];
    }

    return @"Unknown";
}

- (NSString *)cleanModelName:(NSString *)m {
    NSRegularExpression *re = [NSRegularExpression regularExpressionWithPattern:@"\\[\\d+;?\\d*m\\]?"
                                                                        options:0 error:nil];
    NSString *cleaned = [re stringByReplacingMatchesInString:m
                                                      options:0
                                                        range:NSMakeRange(0, [m length])
                                                 withTemplate:@""];
    cleaned = [cleaned stringByTrimmingCharactersInSet:NSCharacterSet.whitespaceCharacterSet];
    return cleaned.length ? cleaned : m;
}

- (NSString *)fetchAPIKey {
    return [self readSettings][@"env"][@"ANTHROPIC_API_KEY"];
}

- (TokenUsage)fetchTodayUsage {
    // Aggregate total from per-model breakdown
    NSDictionary *byModel = [self fetchTodayUsageByModel];
    TokenUsage total = {0,0,0,0};
    for (NSDictionary *d in byModel.allValues) {
        total.input       += [d[@"input"]       integerValue];
        total.output      += [d[@"output"]      integerValue];
        total.cacheRead   += [d[@"cacheRead"]   integerValue];
        total.cacheCreate += [d[@"cacheCreate"] integerValue];
    }
    return total;
}

- (NSDictionary *)fetchTodayUsageByModel {
    NSString *today = [_df stringFromDate:[NSDate date]];
    NSFileManager *fm = NSFileManager.defaultManager;
    // model → @{@"input":NSNumber, @"output":NSNumber, ...}
    NSMutableDictionary *byModel = [NSMutableDictionary dictionary];

    NSArray *projectDirs = [fm contentsOfDirectoryAtPath:_projectsPath error:nil];
    for (NSString *dir in projectDirs) {
        NSString *dirPath = [_projectsPath stringByAppendingPathComponent:dir];
        BOOL isDir = NO;
        if (![fm fileExistsAtPath:dirPath isDirectory:&isDir] || !isDir) continue;

        NSArray *files = [fm contentsOfDirectoryAtPath:dirPath error:nil];
        for (NSString *file in files) {
            if (![file hasSuffix:@".jsonl"]) continue;
            [self accumulateTokensInFile:[dirPath stringByAppendingPathComponent:file]
                                    date:today into:byModel];
        }
    }
    return byModel;
}

- (void)accumulateTokensInFile:(NSString *)path date:(NSString *)date
                          into:(NSMutableDictionary *)byModel {
    NSData *data = [NSData dataWithContentsOfFile:path options:NSDataReadingMappedIfSafe error:nil];
    if (!data) return;

    NSString *content = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
    if (!content) return;

    __block BOOL inToday = NO;
    NSMutableSet *seen = [NSMutableSet set];

    [content enumerateLinesUsingBlock:^(NSString *line, BOOL *stop) {
        if ([line containsString:@"\"timestamp\""] && [line containsString:date]) {
            inToday = YES;
        }
        if (!inToday) return;

        // Extract token fields from usage blocks
        NSInteger inp = -1, out = -1, cr = -1, cc = -1;
        for (NSString *key in @[@"input_tokens", @"output_tokens",
                                @"cache_read_input_tokens", @"cache_creation_input_tokens"]) {
            NSRange r = [line rangeOfString:[NSString stringWithFormat:@"\"%@\":", key]];
            if (r.location != NSNotFound) {
                NSScanner *sc = [NSScanner scannerWithString:
                    [line substringFromIndex:r.location + r.length]];
                NSInteger n = 0;
                if ([sc scanInteger:&n] && n >= 0) {
                    if      ([key isEqual:@"input_tokens"])               inp = n;
                    else if ([key isEqual:@"output_tokens"])              out = n;
                    else if ([key isEqual:@"cache_read_input_tokens"])    cr  = n;
                    else if ([key isEqual:@"cache_creation_input_tokens"]) cc = n;
                }
            }
        }
        if (!(inp >= 0 && out >= 0)) return;

        // Dedup
        NSString *dedupKey = [NSString stringWithFormat:@"%ld,%ld,%ld,%ld",
                               (long)inp, (long)out, (long)cr, (long)cc];
        if ([seen containsObject:dedupKey]) return;
        [seen addObject:dedupKey];

        // Extract model name from this line
        NSString *model = @"unknown";
        NSRange mr = [line rangeOfString:@"\"model\":\""];
        if (mr.location != NSNotFound) {
            NSString *rest = [line substringFromIndex:mr.location + mr.length];
            NSRange mEnd = [rest rangeOfString:@"\""];
            if (mEnd.location != NSNotFound) {
                model = [self cleanModelName:[rest substringToIndex:mEnd.location]];
            }
        }

        // Accumulate into per-model bucket
        NSMutableDictionary *bucket = byModel[model];
        if (!bucket) {
            bucket = [NSMutableDictionary dictionaryWithObjectsAndKeys:
                       @(0), @"input", @(0), @"output",
                       @(0), @"cacheRead", @(0), @"cacheCreate", nil];
            byModel[model] = bucket;
        }
        bucket[@"input"]       = @([bucket[@"input"]       integerValue] + inp);
        bucket[@"output"]      = @([bucket[@"output"]      integerValue] + out);
        bucket[@"cacheRead"]   = @([bucket[@"cacheRead"]   integerValue] + (cr > 0 ? cr : 0));
        bucket[@"cacheCreate"] = @([bucket[@"cacheCreate"] integerValue] + (cc > 0 ? cc : 0));
    }];
}

- (double)computeTotalCostWithModelUsage:(NSDictionary *)modelUsage {
    double total = 0;
    for (NSString *model in modelUsage) {
        NSDictionary *d = modelUsage[model];
        NSInteger inp = [d[@"input"] integerValue];
        NSInteger out = [d[@"output"] integerValue];
        NSInteger cr  = [d[@"cacheRead"] integerValue];
        BOOL isPro = [model.lowercaseString containsString:@"pro"];
        double pIn    = isPro ? kPriceProInput    : kPriceFlashInput;
        double pOut   = isPro ? kPriceProOutput   : kPriceFlashOutput;
        double pCache = isPro ? kPriceProCacheHit : kPriceFlashCacheHit;
        total += (inp * pIn + out * pOut + cr * pCache) / 1000000.0;
    }
    return total;
}

- (void)fetchBalanceWithForce:(BOOL)force completion:(void(^)(double))completion {
    if (!force && _lastBalanceFetch && [[NSDate date] timeIntervalSinceDate:_lastBalanceFetch] < 60) {
        completion(_cachedBalance);
        return;
    }

    NSString *key = [self fetchAPIKey];
    if (!key) { completion(_cachedBalance); return; }

    NSURL *url = [NSURL URLWithString:@"https://api.deepseek.com/user/balance"];
    NSMutableURLRequest *req = [NSMutableURLRequest requestWithURL:url];
    [req setValue:[@"Bearer " stringByAppendingString:key] forHTTPHeaderField:@"Authorization"];
    req.timeoutInterval = 10;

    __weak typeof(self) weakSelf = self;
    [[NSURLSession.sharedSession dataTaskWithRequest:req
        completionHandler:^(NSData *data, NSURLResponse *resp, NSError *err) {
        __strong typeof(weakSelf) self = weakSelf;
        if (!self) { completion(0); return; }

        if (!data) {
            dispatch_async(dispatch_get_main_queue(), ^{ completion(self->_cachedBalance); });
            return;
        }

        NSDictionary *json = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
        double bal = [[json[@"balance_infos"] firstObject][@"total_balance"] doubleValue];

        self->_cachedBalance = bal;
        self->_lastBalanceFetch = NSDate.date;

        dispatch_async(dispatch_get_main_queue(), ^{ completion(bal); });
    }] resume];
}

@end

// ============================================================
// NSPanel subclass — can become key for Touch Bar without activating the app
@interface ClaudeBarPanel : NSPanel @end
@implementation ClaudeBarPanel
- (BOOL)canBecomeKeyWindow { return YES; }
- (BOOL)canBecomeMainWindow { return NO; }
@end

// TouchBarController — manages NSTouchBar + Control Strip
// ============================================================

static NSTouchBarItemIdentifier const kTrayItem  = @"com.claude.tray";
static NSTouchBarItemIdentifier const kModelItem = @"com.claude.model";
static NSTouchBarItemIdentifier const kStatItem  = @"com.claude.stats";

@interface TouchBarController : NSObject <NSTouchBarDelegate>
@property (nonatomic, strong) NSTextField *modelLabel;
@property (nonatomic, strong) NSTextField *tokensLabel;
@property (nonatomic, strong) NSTextField *costLabel;
@property (nonatomic, strong) NSTextField *cacheLabel;
@property (nonatomic, strong) NSTextField *balanceLabel;
@property (readonly) NSTouchBar *claudeBar;
- (void)setup;
- (void)updateWithModel:(NSString *)model tokens:(NSInteger)tokens
                    cost:(double)cost cacheRate:(double)cacheRate balance:(double)balance;
@end

@implementation TouchBarController {
    NSTouchBar *_bar;
    NSWindow *_tbWindow;
}

- (NSTouchBar *)claudeBar { return _bar; }

- (void)setup {
    // ----- Model label (left block) -----
    _modelLabel = [NSTextField labelWithString:@"…"];
    _modelLabel.font = [NSFont monospacedDigitSystemFontOfSize:14 weight:NSFontWeightMedium];
    _modelLabel.textColor = NSColor.labelColor;

    // ----- Stats labels (right block, 2x2 grid) -----
    _tokensLabel = [NSTextField labelWithString:@"…"];
    _tokensLabel.font = [NSFont monospacedDigitSystemFontOfSize:12 weight:NSFontWeightSemibold];
    _tokensLabel.textColor = NSColor.labelColor;

    _costLabel = [NSTextField labelWithString:@""];
    _costLabel.font = [NSFont monospacedDigitSystemFontOfSize:12 weight:NSFontWeightRegular];
    _costLabel.textColor = NSColor.labelColor;

    _cacheLabel = [NSTextField labelWithString:@""];
    _cacheLabel.font = [NSFont monospacedDigitSystemFontOfSize:12 weight:NSFontWeightRegular];
    _cacheLabel.textColor = NSColor.labelColor;

    _balanceLabel = [NSTextField labelWithString:@""];
    _balanceLabel.font = [NSFont monospacedDigitSystemFontOfSize:12 weight:NSFontWeightRegular];
    _balanceLabel.textColor = NSColor.labelColor;

    // ----- Touch Bar -----
    _bar = [[NSTouchBar alloc] init];
    _bar.delegate = self;
    _bar.defaultItemIdentifiers = @[kModelItem, kStatItem, NSTouchBarItemIdentifierFlexibleSpace];
    _bar.customizationIdentifier = @"com.claude.bar";

    // NSPanel with NonactivatingPanel — becomes key for Touch Bar without stealing focus
    _tbWindow = [[ClaudeBarPanel alloc] initWithContentRect:NSMakeRect(0, 0, 500, 44)
                                           styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
                                             backing:NSBackingStoreBuffered defer:NO];
    _tbWindow.releasedWhenClosed = NO;
    _tbWindow.touchBar = _bar;
    _tbWindow.level = NSFloatingWindowLevel;
    _tbWindow.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces
                                 | NSWindowCollectionBehaviorIgnoresCycle
                                 | NSWindowCollectionBehaviorFullScreenAuxiliary;
    _tbWindow.ignoresMouseEvents = YES;
    _tbWindow.opaque = NO;
    _tbWindow.backgroundColor = [NSColor colorWithWhite:0.05 alpha:0.75];
    _tbWindow.titlebarAppearsTransparent = YES;

    // Labels in the panel
    NSView *cv = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 500, 44)];
    cv.wantsLayer = YES;
    cv.layer.cornerRadius = 8;
    _modelLabel.frame   = NSMakeRect(12, 12, 190, 20);
    _tokensLabel.frame  = NSMakeRect(215, 26, 130, 16);
    _costLabel.frame    = NSMakeRect(215, 6,  130, 16);
    _cacheLabel.frame   = NSMakeRect(345, 26, 130, 16);
    _balanceLabel.frame = NSMakeRect(345, 6,  145, 16);
    for (NSView *v in @[_modelLabel, _tokensLabel, _costLabel, _cacheLabel, _balanceLabel]) {
        [cv addSubview:v];
    }
    _tbWindow.contentView = cv;

    // Keep offscreen — only needed as a key window for Touch Bar
    [_tbWindow setFrameOrigin:NSMakePoint(-9999, -9999)];
    [_tbWindow orderFront:nil];

    // Present as system modal so it stays visible across app switches
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 1 * NSEC_PER_SEC),
                   dispatch_get_main_queue(), ^{
        LoadDFRFoundation();
        if (DFRSetPresence) DFRSetPresence((__bridge CFStringRef)kTrayItem, true);
    if (DFRSetCloseBox) DFRSetCloseBox(false);
        SEL psSel = NSSelectorFromString(@"presentSystemModalTouchBar:systemTrayItemIdentifier:");
        if ([NSTouchBar respondsToSelector:psSel]) {
            ((void(*)(id, SEL, id, id))objc_msgSend)([NSTouchBar class], psSel, _bar, kTrayItem);
        }
    });
}

- (void)showTouchBar {
    [_tbWindow makeKeyAndOrderFront:nil];
}

- (void)dealloc {
    if (DFRSetPresence) DFRSetPresence((__bridge CFStringRef)kTrayItem, false);
}

- (void)updateWithModel:(NSString *)model tokens:(NSInteger)tokens
                    cost:(double)cost cacheRate:(double)cacheRate balance:(double)balance {
    NSString *ts;
    if      (tokens >= 1000000) ts = [NSString stringWithFormat:@"%.1fM", tokens/1000000.0];
    else if (tokens >= 1000)    ts = [NSString stringWithFormat:@"%.1fK", tokens/1000.0];
    else                        ts = [NSString stringWithFormat:@"%ld", (long)tokens];

    _modelLabel.stringValue   = model ?: @"";
    _tokensLabel.stringValue  = [NSString stringWithFormat:@"Tokens: %@", ts];
    _costLabel.stringValue    = cost > 0.01 ? [NSString stringWithFormat:@"Cost: ¥%.2f", cost] : @"Cost: ¥0.00";
    _cacheLabel.stringValue   = cacheRate > 0 ? [NSString stringWithFormat:@"Cache: %.0f%%", cacheRate] : @"";
    _balanceLabel.stringValue = [NSString stringWithFormat:@"Balance: ¥%.2f", balance];
}

#pragma mark - NSTouchBarDelegate

- (NSTouchBarItem *)touchBar:(NSTouchBar *)bar makeItemForIdentifier:(NSTouchBarItemIdentifier)ident {
    if ([ident isEqual:kModelItem]) {
        NSCustomTouchBarItem *item = [[NSCustomTouchBarItem alloc] initWithIdentifier:ident];
        item.view = _modelLabel;
        return item;
    }
    if ([ident isEqual:kStatItem]) {
        NSCustomTouchBarItem *item = [[NSCustomTouchBarItem alloc] initWithIdentifier:ident];

        // 2x2 grid: left column = tokens + cache, right column = cost + balance
        NSStackView *leftCol  = [NSStackView stackViewWithViews:@[_tokensLabel, _cacheLabel]];
        leftCol.orientation  = NSUserInterfaceLayoutOrientationVertical;
        leftCol.spacing      = 2;
        leftCol.alignment    = NSLayoutAttributeLeading;

        NSStackView *rightCol = [NSStackView stackViewWithViews:@[_costLabel, _balanceLabel]];
        rightCol.orientation = NSUserInterfaceLayoutOrientationVertical;
        rightCol.spacing     = 2;
        rightCol.alignment   = NSLayoutAttributeLeading;

        NSStackView *grid = [NSStackView stackViewWithViews:@[leftCol, rightCol]];
        grid.orientation  = NSUserInterfaceLayoutOrientationHorizontal;
        grid.spacing      = 16;

        item.view = grid;
        return item;
    }
    return nil;
}

@end

// Forward declaration for FSEvents callback
static void onClaudeDirChanged(ConstFSEventStreamRef, void *, size_t,
                                void *, const FSEventStreamEventFlags[],
                                const FSEventStreamEventId[]);
// Parses the desktop app's LevelDB to get the currently selected model
static NSString *desktopModelFromLevelDB(void);
// Maps desktop model names (e.g. claude-sonnet-4-6) to actual model names
static NSString *resolveDesktopModel(NSString *desktopModel);

// ============================================================
// AppDelegate — menu bar + lifecycle + periodic refresh
// ============================================================

@interface AppDelegate : NSObject <NSApplicationDelegate, NSMenuDelegate>
@property (nonatomic, strong) DataFetcher          *fetcher;
@property (nonatomic, strong) TouchBarController   *touchBar;
@property (nonatomic, strong) NSStatusItem         *statusItem;
- (void)refreshModelOnly;
- (void)updateDesktopModel:(NSString *)model;
@end

// Key event tracking for auto-dismiss on typing - IOKit HID monitoring
// (No accessibility permissions needed)
// Uses CGEventSourceSecondsSinceLastKeyPress - no permissions needed

@implementation AppDelegate {
    NSString           *_lastModel;
    TokenUsage          _lastUsage;
    NSDictionary       *_lastUsageByModel;  // per-model breakdown for cost calc
    double              _lastCost;
    double              _lastCacheRate;
    double              _lastBalance;
    FSEventStreamRef    _fsStream;
    NSTimer            *_refreshTimer;
    NSTimer            *_typingTimer;
    BOOL                _pinned;
    BOOL                _dismissedForTyping;
    NSMenuItem         *_pinMenuItem;
}

- (void)applicationDidFinishLaunching:(NSNotification *)note {
    _fetcher  = [[DataFetcher alloc] init];
    _touchBar = [[TouchBarController alloc] init];

    // --- Menu bar ---
    _statusItem = [NSStatusBar.systemStatusBar statusItemWithLength:NSVariableStatusItemLength];
    _statusItem.button.font = [NSFont monospacedDigitSystemFontOfSize:10 weight:NSFontWeightMedium];

    NSMenu *menu = [[NSMenu alloc] init];
    menu.delegate = self;
    [menu addItemWithTitle:@"Model: …" action:nil keyEquivalent:@""];
    [menu addItemWithTitle:@"Today: …" action:nil keyEquivalent:@""];
    [menu addItemWithTitle:@"Cost: …" action:nil keyEquivalent:@""];
    [menu addItemWithTitle:@"Balance: …" action:nil keyEquivalent:@""];
    [menu addItem:NSMenuItem.separatorItem];

    NSMenuItem *refreshItem = [menu addItemWithTitle:@"Refresh Now" action:@selector(refreshNow) keyEquivalent:@""];
    refreshItem.target = self;

    _pinned = YES;
    _pinMenuItem = [menu addItemWithTitle:@"▸ Pin on Touch Bar" action:@selector(togglePinTouchBar) keyEquivalent:@""];
    _pinMenuItem.target = self;

    [menu addItem:NSMenuItem.separatorItem];

    NSMenuItem *quitItem = [menu addItemWithTitle:@"Quit" action:@selector(quitApp) keyEquivalent:@"q"];
    quitItem.target = self;

    _statusItem.menu = menu;

    // --- Touch Bar ---
    [_touchBar setup];
    // NSTouchBar on the status button makes it visible during menu tracking
    _statusItem.button.touchBar = _touchBar.claudeBar;

    // --- Refresh: local data every 30s, balance API every 5 min ---
    [self refreshWithForceBalance:YES];
    _refreshTimer = [NSTimer scheduledTimerWithTimeInterval:30 repeats:YES block:^(NSTimer *t) {
        [self refreshWithForceBalance:NO];
    }];

    // --- FSEvents watch .claude/ AND Claude Desktop for model changes ---
    CFStringRef watchPaths[] = {
        (__bridge CFStringRef)[@"~/.claude" stringByExpandingTildeInPath],
        (__bridge CFStringRef)[@"~/Library/Application Support/Claude-3p/Local Storage/leveldb"
                               stringByExpandingTildeInPath],
    };
    CFArrayRef paths = CFArrayCreate(NULL, (const void **)watchPaths, 2, &kCFTypeArrayCallBacks);
    FSEventStreamContext ctx = {0, (__bridge void *)self, NULL, NULL, NULL};
    _fsStream = FSEventStreamCreate(NULL, &onClaudeDirChanged, &ctx,
                                    paths, kFSEventStreamEventIdSinceNow,
                                    0.3,  // latency
                                    kFSEventStreamCreateFlagFileEvents);
    if (_fsStream) {
        FSEventStreamSetDispatchQueue(_fsStream, dispatch_get_main_queue());
        FSEventStreamStart(_fsStream);
    }
    CFRelease(paths);



    // Poll every 1s: check time since last key press
    // CGEventSourceSecondsSinceLastKeyPress returns 0-999 sec since any keyboard activity
    _typingTimer = [NSTimer scheduledTimerWithTimeInterval:1 repeats:YES block:^(NSTimer *t) {
        if (!_pinned) return;
        double elapsed = CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateCombinedSessionState, kCGEventKeyDown);
        if (elapsed < 2.0 && !_dismissedForTyping) {
            SEL sel = NSSelectorFromString(@"dismissSystemModalTouchBar:");
            if ([NSTouchBar respondsToSelector:sel])
                ((void(*)(id, SEL, id))objc_msgSend)([NSTouchBar class], sel, _touchBar.claudeBar);
            _dismissedForTyping = YES;
        } else if (elapsed > 5.0 && _dismissedForTyping) {
            [self presentTouchBarModal];
            _dismissedForTyping = NO;
        }
    }];

    // Refresh on wake
    [NSWorkspace.sharedWorkspace.notificationCenter addObserver:self
        selector:@selector(refreshOnWake) name:NSWorkspaceDidWakeNotification object:nil];
}

- (void)refreshOnWake {
    [self refreshWithForceBalance:YES];
}

- (void)refreshWithForceBalance:(BOOL)force {
    // Prefer Claude Code model from settings.json, fallback to Desktop App model
    _lastModel   = [_fetcher fetchModel];
    if (!_lastModel || [_lastModel isEqualToString:@"Unknown"]) {
        NSString *dm = desktopModelFromLevelDB();
        if (dm) dm = resolveDesktopModel(dm);
        if (dm) _lastModel = dm;
    }
    _lastUsageByModel = [_fetcher fetchTodayUsageByModel];
    // Aggregate total from per-model breakdown (avoids re-parsing everything)
    _lastUsage = (TokenUsage){0,0,0,0};
    for (NSDictionary *d in _lastUsageByModel.allValues) {
        _lastUsage.input       += [d[@"input"]       integerValue];
        _lastUsage.output      += [d[@"output"]      integerValue];
        _lastUsage.cacheRead   += [d[@"cacheRead"]   integerValue];
        _lastUsage.cacheCreate += [d[@"cacheCreate"] integerValue];
    }
    _lastCost    = [_fetcher computeTotalCostWithModelUsage:_lastUsageByModel];

    // Cache hit rate: cache_read / (input + cache_read + cache_create)
    NSInteger totalCached = _lastUsage.input + _lastUsage.cacheRead + _lastUsage.cacheCreate;
    _lastCacheRate = totalCached > 0 ? (double)_lastUsage.cacheRead / totalCached * 100.0 : 0;

    __weak typeof(self) ws = self;
    [_fetcher fetchBalanceWithForce:force completion:^(double bal) {
        __strong typeof(ws) ss = ws;
        if (!ss) return;
        ss->_lastBalance = bal;
        [ss refreshUI];
    }];
    [self refreshUI];
}

- (void)refreshUI {
    // Total = input + output + cache_read (matching DeepSeek platform's "总消耗")
    NSInteger totalTokens = _lastUsage.input + _lastUsage.output + _lastUsage.cacheRead;
    NSString *tks = [self formatNumber:totalTokens];

    _statusItem.button.title = [NSString stringWithFormat:@"CC %@", tks];

    [_statusItem.menu itemAtIndex:0].title = [NSString stringWithFormat:@"Model: %@", _lastModel];
    [_statusItem.menu itemAtIndex:1].title = [NSString stringWithFormat:
        @"Today: %@ tokens  (in %@ | out %@ | cache +%@)",
        tks,
        [self formatNumber:_lastUsage.input],
        [self formatNumber:_lastUsage.output],
        [self formatNumber:_lastUsage.cacheRead + _lastUsage.cacheCreate]];
    [_statusItem.menu itemAtIndex:2].title = [NSString stringWithFormat:
        @"Cost: ¥%.2f  |  Cache hit: %.0f%% (%@ read)", _lastCost, _lastCacheRate,
        [self formatNumber:_lastUsage.cacheRead]];
    [_statusItem.menu itemAtIndex:3].title = [NSString stringWithFormat:@"Balance: ¥%.2f", _lastBalance];

    [_touchBar updateWithModel:_lastModel tokens:totalTokens
                          cost:_lastCost cacheRate:_lastCacheRate balance:_lastBalance];
}

- (NSString *)formatNumber:(NSInteger)n {
    if      (n >= 1000000) return [NSString stringWithFormat:@"%.1fM", n/1000000.0];
    else if (n >= 1000)    return [NSString stringWithFormat:@"%.1fK", n/1000.0];
    else                   return [NSString stringWithFormat:@"%ld", (long)n];
}

- (void)refreshModelOnly {
    _lastModel = [_fetcher fetchModel];
    if (!_lastModel || [_lastModel isEqualToString:@"Unknown"]) {
        NSString *dm = desktopModelFromLevelDB();
        if (dm) dm = resolveDesktopModel(dm);
        if (dm) _lastModel = dm;
    }
    if (_lastUsageByModel) {
        _lastCost = [_fetcher computeTotalCostWithModelUsage:_lastUsageByModel];
    }
    [self refreshUI];
}

- (void)updateDesktopModel:(NSString *)model {
    _lastModel = [model copy];
    if (_lastUsageByModel) {
        _lastCost = [_fetcher computeTotalCostWithModelUsage:_lastUsageByModel];
    }
    [self refreshUI];
}

- (void)refreshNow {
    [self.touchBar showTouchBar];
    [self refreshWithForceBalance:YES];
}

- (void)presentTouchBarModal {
    LoadDFRFoundation();
    if (DFRSetPresence) DFRSetPresence((__bridge CFStringRef)kTrayItem, true);
    if (DFRSetCloseBox) DFRSetCloseBox(false);
    SEL sel = NSSelectorFromString(@"presentSystemModalTouchBar:systemTrayItemIdentifier:");
    if ([NSTouchBar respondsToSelector:sel])
        ((void(*)(id, SEL, id, id))objc_msgSend)([NSTouchBar class], sel, _touchBar.claudeBar, kTrayItem);
}

- (void)togglePinTouchBar {
    _pinned = !_pinned;
    _pinMenuItem.title = _pinned ? @"▸ Pin on Touch Bar" : @"  Pin on Touch Bar";

    if (_pinned) {
        [self presentTouchBarModal];
        _dismissedForTyping = NO;
    } else {
        SEL sel = NSSelectorFromString(@"dismissSystemModalTouchBar:");
        if ([NSTouchBar respondsToSelector:sel])
            ((void(*)(id, SEL, id))objc_msgSend)([NSTouchBar class], sel, _touchBar.claudeBar);
    }
}

- (void)menuWillOpen:(NSMenu *)menu {
    [self.touchBar showTouchBar];
}

- (void)quitApp { [NSApp terminate:nil]; }

- (void)dealloc {
    [_refreshTimer invalidate];
    [_typingTimer invalidate];
    [NSWorkspace.sharedWorkspace.notificationCenter removeObserver:self];
    if (_fsStream) {
        FSEventStreamStop(_fsStream);
        FSEventStreamInvalidate(_fsStream);
        FSEventStreamRelease(_fsStream);
    }
}

@end

// ============================================================
// FSEvents callback — settings.json changes → instant model update
// ============================================================

static void onClaudeDirChanged(ConstFSEventStreamRef stream, void *info,
                                size_t num, void *paths,
                                const FSEventStreamEventFlags flags[],
                                const FSEventStreamEventId ids[]) {
    __unsafe_unretained AppDelegate *delegate = (__bridge AppDelegate *)info;
    char **pathList = (char **)paths;
    for (size_t i = 0; i < num; i++) {
        NSString *p = [NSString stringWithUTF8String:pathList[i]];
        if ([p hasSuffix:@"settings.json"]) {
            dispatch_async(dispatch_get_main_queue(), ^{
                [delegate refreshModelOnly];
            });
            return;
        }
        // Claude Desktop LevelDB changed → detect model
        if ([p containsString:@"leveldb"]) {
            dispatch_async(dispatch_get_main_queue(), ^{
                NSString *m = desktopModelFromLevelDB();
                if (m) m = resolveDesktopModel(m);
                if (m) [delegate updateDesktopModel:m];
            });
            return;
        }
    }
}

// ============================================================
// Desktop App model detection
// ============================================================

static NSString *desktopModelFromLevelDB(void) {
    NSString *dir = [@"~/Library/Application Support/Claude-3p/Local Storage/leveldb"
                      stringByExpandingTildeInPath];
    NSFileManager *fm = NSFileManager.defaultManager;
    NSArray *files = [fm contentsOfDirectoryAtPath:dir error:nil];
    if (!files) return nil;

    // Search key we care about
    NSString *searchKey = @"cowork-sticky-model-selector-org-00000000-0000-4000-8000-000000000001";
    NSData *keyData = [searchKey dataUsingEncoding:NSUTF8StringEncoding];

    for (NSString *f in files) {
        if (![f hasSuffix:@".log"]) continue;
        NSData *data = [NSData dataWithContentsOfFile:[dir stringByAppendingPathComponent:f]
                                              options:NSDataReadingMappedIfSafe error:nil];
        if (!data) continue;

        // Search from end to find the most recent write
        NSRange r = [data rangeOfData:keyData options:NSDataSearchBackwards
                                range:NSMakeRange(0, data.length)];
        if (r.location == NSNotFound) continue;

        // Model name appears shortly after the key, skip past key + delimiter bytes
        NSUInteger pos = r.location + r.length;
        // Scan forward looking for "claude-" prefix
        const uint8_t *bytes = data.bytes;
        for (NSUInteger i = pos; i < data.length - 8; i++) {
            if (bytes[i] == 'c' && bytes[i+1] == 'l' && bytes[i+2] == 'a') {
                NSUInteger end = i;
                while (end < data.length && bytes[end] >= ' ' && bytes[end] != ']') end++;
                return [[NSString alloc] initWithBytes:bytes + i length:end - i
                                             encoding:NSUTF8StringEncoding];
            }
        }
    }
    return nil;
}

static NSString *resolveDesktopModel(NSString *desktopModel) {
    if (!desktopModel) return nil;
    // Read known configLibrary file to get labelOverride mapping
    NSString *cfgDir = [@"~/Library/Application Support/Claude-3p/configLibrary"
                         stringByExpandingTildeInPath];
    NSFileManager *fm = NSFileManager.defaultManager;
    NSArray *files = [fm contentsOfDirectoryAtPath:cfgDir error:nil];
    for (NSString *f in files) {
        NSData *d = [NSData dataWithContentsOfFile:[cfgDir stringByAppendingPathComponent:f]];
        if (!d) continue;
        NSDictionary *json = [NSJSONSerialization JSONObjectWithData:d options:0 error:nil];
        for (NSDictionary *m in json[@"inferenceModels"]) {
            if ([m[@"name"] isEqual:desktopModel]) {
                NSString *override = m[@"labelOverride"];
                if (override) return override;
            }
        }
    }
    // Fallback: strip ANSI artifacts like [1m
    NSRegularExpression *re = [NSRegularExpression regularExpressionWithPattern:@"\\[\\d+;?\\d*m\\]?"
                                                                        options:0 error:nil];
    return [re stringByReplacingMatchesInString:desktopModel options:0
                                         range:NSMakeRange(0, desktopModel.length)
                                  withTemplate:@""];
}

// ============================================================
int main(int argc, const char *argv[]) {
    @autoreleasepool {
        NSApplication *app = NSApplication.sharedApplication;
        AppDelegate *delegate = [[AppDelegate alloc] init];
        app.delegate = delegate;   // NSApp retains delegate; false-positive warning
        [app run];
    }
    return 0;
}
