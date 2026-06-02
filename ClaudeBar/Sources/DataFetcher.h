// DataFetcher.h

#import <Foundation/Foundation.h>

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
- (NSDictionary *)fetchTodayUsageByModel;
- (double)computeTotalCostWithModelUsage:(NSDictionary *)modelUsage;
- (void)fetchBalanceWithForce:(BOOL)force completion:(void(^)(double))completion;
@property (nonatomic, readonly) NSString *projectsPath;
- (NSArray *)fetchDailyUsageForLastDays:(int)days;
- (NSArray *)fetchProjectRanking;
- (NSArray *)fetchAllDashboardData;  // @[@[dailyTotals], @[ranking]]
@end
