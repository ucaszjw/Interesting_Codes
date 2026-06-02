// Dashboard.h — popup panel with 7-day chart + project ranking

#import <Cocoa/Cocoa.h>
#import "DataFetcher.h"

@interface DashboardController : NSWindowController
- (instancetype)initWithDataFetcher:(DataFetcher *)fetcher;
- (void)showRelativeToRect:(NSRect)rect ofView:(NSView *)view;
@property (nonatomic, copy) void (^onActivate)(void);
@end
