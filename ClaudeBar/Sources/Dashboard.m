// Dashboard.m — 7-day chart + project ranking panel

#import "Dashboard.h"
#import <Cocoa/Cocoa.h>

// ============================================================
// ChartView — Core Graphics 7-day line chart
// ============================================================
@interface ChartView : NSView
@property (nonatomic, strong) NSArray *dailyTotals;
@end

@implementation ChartView {
    NSDictionary *_attribs;
}

- (instancetype)initWithFrame:(NSRect)frame {
    if (self = [super initWithFrame:frame]) {
        _attribs = @{NSFontAttributeName: [NSFont monospacedDigitSystemFontOfSize:10 weight:NSFontWeightRegular],
                     NSForegroundColorAttributeName: [NSColor secondaryLabelColor]};
    }
    return self;
}

- (void)setDailyTotals:(NSArray *)dailyTotals {
    _dailyTotals = dailyTotals;
    [self setNeedsDisplay:YES];
}

- (void)drawRect:(NSRect)dirtyRect {
    if (_dailyTotals.count < 2) return;

    CGFloat w = self.bounds.size.width;
    CGFloat h = self.bounds.size.height;
    CGFloat padL = 45, padR = 10, padT = 10, padB = 30;
    CGFloat plotW = w - padL - padR;
    CGFloat plotH = h - padT - padB;

    double maxVal = 1;
    for (NSNumber *n in _dailyTotals) maxVal = MAX(maxVal, n.doubleValue);
    maxVal = ceil(maxVal * 1.2 / 1000) * 1000;

    [[NSColor.darkGrayColor colorWithAlphaComponent:0.3] setStroke];
    NSBezierPath *grid = [NSBezierPath bezierPath];
    for (int i = 0; i <= 4; i++) {
        CGFloat y = padB + plotH * i / 4.0;
        [grid moveToPoint:NSMakePoint(padL, y)];
        [grid lineToPoint:NSMakePoint(w - padR, y)];
        NSString *label = [self shortStr:maxVal * i / 4.0];
        [label drawAtPoint:NSMakePoint(2, y - 6) withAttributes:_attribs];
    }
    [grid stroke];

    NSUInteger n = _dailyTotals.count;
    double *px = malloc(n * sizeof(double));
    double *py = malloc(n * sizeof(double));
    for (NSUInteger i = 0; i < n; i++) {
        px[i] = padL + plotW * i / (n - 1);
        py[i] = padB + plotH * [_dailyTotals[i] doubleValue] / maxVal;
    }

    NSBezierPath *area = [NSBezierPath bezierPath];
    [area moveToPoint:NSMakePoint(px[0], padB)];
    for (NSUInteger i = 0; i < n; i++) [area lineToPoint:NSMakePoint(px[i], py[i])];
    [area lineToPoint:NSMakePoint(px[n-1], padB)];
    [area closePath];
    [[NSColor.systemBlueColor colorWithAlphaComponent:0.12] setFill];
    [area fill];

    NSBezierPath *line = [NSBezierPath bezierPath];
    [line moveToPoint:NSMakePoint(px[0], py[0])];
    for (NSUInteger i = 1; i < n; i++) [line lineToPoint:NSMakePoint(px[i], py[i])];
    line.lineWidth = 2;
    [[NSColor systemBlueColor] setStroke];
    [line stroke];

    for (NSUInteger i = 0; i < n; i++) {
        NSBezierPath *dot = [NSBezierPath bezierPathWithOvalInRect:NSMakeRect(px[i]-3, py[i]-3, 6, 6)];
        [NSColor.systemBlueColor setFill];
        [dot fill];
    }

    NSDateFormatter *df = [[NSDateFormatter alloc] init];
    df.dateFormat = @"E";
    for (NSUInteger i = 0; i < n; i++) {
        NSDate *d = [NSDate dateWithTimeIntervalSinceNow:-(n - 1 - i) * 86400];
        CGFloat x = px[i] - 12;
        if (i == 0) x = padL;
        if (i == n-1) x = w - padR - 20;
        [[df stringFromDate:d] drawAtPoint:NSMakePoint(x, 4) withAttributes:_attribs];
    }
    free(px); free(py);
}

- (NSString *)shortStr:(double)v {
    if (v >= 1000000) return [NSString stringWithFormat:@"%.0fM", v/1000000];
    if (v >= 1000)    return [NSString stringWithFormat:@"%.0fK", v/1000];
    return [NSString stringWithFormat:@"%.0f", v];
}
@end

// Prevent dashboard from stealing key window (which hides Touch Bar)
@interface DashboardPanel : NSPanel @end
@implementation DashboardPanel
- (BOOL)canBecomeKeyWindow { return NO; }
@end

// ============================================================
// DashboardController
// ============================================================
@interface DashboardController () <NSTableViewDataSource, NSTableViewDelegate>
@property (nonatomic, strong) DataFetcher *fetcher;
@property (nonatomic, strong) ChartView *chartView;
@property (nonatomic, strong) NSTableView *rankingTable;
@property (nonatomic, strong) NSArray *rankingData;
@end

@implementation DashboardController

- (instancetype)initWithDataFetcher:(DataFetcher *)fetcher {
    self = [super initWithWindow:nil];
    if (self) { _fetcher = fetcher; _rankingData = @[]; }
    return self;
}

- (void)showRelativeToRect:(NSRect)rect ofView:(NSView *)view {
    [self setupWindow];
    [self loadData];
    [self showWindow:nil];

    NSWindow *win = self.window;
    // Convert button frame to screen coordinates
    NSRect screenRect = [view.window convertRectToScreen:rect];
    NSPoint origin = NSMakePoint(NSMidX(screenRect) - win.frame.size.width / 2,
                                  screenRect.origin.y - win.frame.size.height - 4);
    [win setFrameOrigin:origin];
    [win makeKeyAndOrderFront:nil];
}

- (void)loadData {
    DataFetcher *fetcher = _fetcher;
    __weak typeof(self) ws = self;
    dispatch_async(dispatch_get_global_queue(DISPATCH_QUEUE_PRIORITY_DEFAULT, 0), ^{
        NSArray *data = [fetcher fetchAllDashboardData];
        dispatch_async(dispatch_get_main_queue(), ^{
            __strong typeof(ws) ss = ws;
            if (!ss) return;
            ss->_chartView.dailyTotals = data[0];
            ss->_rankingData = data[1];
            [ss->_rankingTable reloadData];
        });
    });
}

- (void)setupWindow {
    if (self.window) return;

    NSWindow *win = [[DashboardPanel alloc] initWithContentRect:NSMakeRect(0, 0, 380, 420)
                                                       styleMask:NSWindowStyleMaskTitled |
                                                                 NSWindowStyleMaskClosable |
                                                                 NSWindowStyleMaskNonactivatingPanel
                                                 backing:NSBackingStoreBuffered defer:NO];
    win.title = @"Claude Code Usage";
    win.level = NSFloatingWindowLevel;
    win.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces |
                             NSWindowCollectionBehaviorTransient;
    win.backgroundColor = [NSColor windowBackgroundColor];
    self.window = win;

    NSView *cv = win.contentView;

    _chartView = [[ChartView alloc] initWithFrame:NSMakeRect(0, 220, 380, 180)];
    [cv addSubview:_chartView];

    NSScrollView *scroll = [[NSScrollView alloc] initWithFrame:NSMakeRect(10, 10, 360, 200)];
    _rankingTable = [[NSTableView alloc] initWithFrame:scroll.bounds];
    _rankingTable.dataSource = self;
    _rankingTable.delegate = self;
    _rankingTable.headerView = nil;
    _rankingTable.rowHeight = 18;
    _rankingTable.selectionHighlightStyle = NSTableViewSelectionHighlightStyleNone;
    _rankingTable.backgroundColor = NSColor.clearColor;

    NSTableColumn *nameCol = [[NSTableColumn alloc] initWithIdentifier:@"name"];
    [nameCol setWidth:200];
    [_rankingTable addTableColumn:nameCol];

    NSTableColumn *tokenCol = [[NSTableColumn alloc] initWithIdentifier:@"tokens"];
    [tokenCol setWidth:90];
    [[tokenCol headerCell] setAlignment:NSTextAlignmentRight];
    [_rankingTable addTableColumn:tokenCol];

    NSTableColumn *costCol = [[NSTableColumn alloc] initWithIdentifier:@"cost"];
    [costCol setWidth:80];
    [[costCol headerCell] setAlignment:NSTextAlignmentRight];
    [_rankingTable addTableColumn:costCol];

    scroll.documentView = _rankingTable;
    scroll.borderType = NSNoBorder;
    scroll.drawsBackground = NO;
    [cv addSubview:scroll];
}

- (NSInteger)numberOfRowsInTableView:(NSTableView *)tv { return _rankingData.count; }

- (NSView *)tableView:(NSTableView *)tv viewForTableColumn:(NSTableColumn *)col row:(NSInteger)row {
    NSDictionary *item = _rankingData[row];
    NSString *key = [col identifier];
    NSString *text = item[key] ?: @"";
    if ([key isEqual:@"tokens"]) text = [self shortStr:[item[@"tokens"] doubleValue]];
    if ([key isEqual:@"cost"])   text = [NSString stringWithFormat:@"¥%.2f", [item[@"cost"] doubleValue]];

    NSTextField *tf = [NSTextField labelWithString:text];
    [tf setFont:[NSFont monospacedDigitSystemFontOfSize:11 weight:NSFontWeightRegular]];
    [tf setTextColor:[key isEqual:@"name"] ? NSColor.labelColor : NSColor.secondaryLabelColor];
    if ([key isEqual:@"name"]) {
        [tf setLineBreakMode:NSLineBreakByTruncatingMiddle];
        [tf setToolTip:text];
    }
    return tf;
}

- (NSString *)shortStr:(double)v {
    if (v >= 1000000) return [NSString stringWithFormat:@"%.1fM", v/1000000];
    if (v >= 1000)    return [NSString stringWithFormat:@"%.1fK", v/1000];
    return [NSString stringWithFormat:@"%.0f", v];
}
@end
