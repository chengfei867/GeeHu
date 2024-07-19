package logic

import (
	"context"
	"strconv"
	"time"

	"beyond/application/follow/code"
	"beyond/application/follow/rpc/internal/model"
	"beyond/application/follow/rpc/internal/svc"
	"beyond/application/follow/rpc/internal/types"
	"beyond/application/follow/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"
)

const userFansExpireTime = 3600 * 24 * 2

type FansListLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewFansListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FansListLogic {
	return &FansListLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// FansList 粉丝列表.
func (l *FansListLogic) FansList(in *pb.FansListRequest) (*pb.FansListResponse, error) {
	if in.UserId == 0 {
		return nil, code.UserIdEmpty
	}
	if in.PageSize == 0 {
		in.PageSize = types.DefaultPageSize
	}
	if in.Cursor == 0 {
		in.Cursor = time.Now().Unix()
	}
	var (
		err            error
		isCache, isEnd bool
		lastId, cursor int64
		fansUserIds    []int64
		fansModel      []*model.Follow
		curPage        []*pb.FansItem
	)
	//先从缓存中查看是否有
	fansUIds, createTime, _ := l.cacheFansUserIds(l.ctx, in.UserId, in.Cursor, in.PageSize)
	if len(fansUIds) > 0 {
		isCache = true
		//说明列表到头了
		if fansUIds[len(fansUIds)-1] == -1 {
			fansUIds = fansUIds[:len(fansUIds)-1]
			isEnd = true
		}
		fansUserIds = fansUIds
		for i, fansUId := range fansUIds {
			curPage = append(curPage, &pb.FansItem{
				UserId:     in.UserId,
				FansUserId: fansUId,
				CreateTime: createTime[i],
			})
		}
	} else {
		//缓存中没有数据 数据库中查 查出来的是第一页的粉丝
		fansModel, err = l.svcCtx.FollowModel.FindByFollowedUserId(l.ctx, in.UserId, types.CacheMaxFansCount)
		if err != nil {
			l.Logger.Errorf("[FansList] FollowModel.FindByFollowedUserId error: %v req: %v", err, in)
			return nil, err
		}
		if len(fansModel) == 0 {
			return &pb.FansListResponse{}, nil
		}
		// 存放的是该用户的粉丝列表
		var firstPageFans []*model.Follow
		if len(fansModel) > int(in.PageSize) {
			firstPageFans = fansModel[:in.PageSize]
		} else {
			firstPageFans = fansModel
			isEnd = true
		}

		for _, fans := range firstPageFans {
			//粉丝的id
			fansUserIds = append(fansUserIds, fans.UserID)
			//当前页的信息
			curPage = append(curPage, &pb.FansItem{
				//当前粉丝的被关注id 实际上就是userid
				UserId: fans.FollowedUserID,
				//粉丝id 就是这个粉丝的用户id
				FansUserId: fans.UserID,
				CreateTime: fans.CreateTime.Unix(),
			})
		}
	}
	if len(curPage) > 0 {
		pageLast := curPage[len(curPage)-1]
		lastId = pageLast.FansUserId
		cursor = pageLast.CreateTime
		if cursor < 0 {
			cursor = 0
		}
		//去重
		for i, fans := range curPage {
			if fans.CreateTime == in.Cursor && fans.FansUserId == in.Id {
				curPage = curPage[i:]
				break
			}
		}
	}
	//从粉丝数列表中查询出粉丝的粉丝数量 和关注数量
	fa, err := l.svcCtx.FollowCountModel.FindByUserIds(l.ctx, fansUserIds)
	if err != nil {
		l.Logger.Errorf("[FansList] FollowCountModel.FindByUserIds error: %v fansUserIds: %v", err, fansUserIds)
	}
	uidFansCount := make(map[int64]int)
	uidFollowCount := make(map[int64]int)
	for _, f := range fa {
		//该粉丝的粉丝数量
		uidFansCount[f.UserID] = f.FansCount
		//该粉丝的关注数量
		uidFollowCount[f.UserID] = f.FollowCount
	}
	for _, cur := range curPage {
		cur.FansCount = int64(uidFansCount[cur.FansUserId])
		cur.FollowCount = int64(uidFollowCount[cur.FansUserId])
	}

	ret := &pb.FansListResponse{
		Items:  curPage,
		Cursor: cursor,
		IsEnd:  isEnd,
		Id:     lastId,
	}

	//异步写缓存
	if !isCache {
		threading.GoSafe(func() {
			if len(fansModel) < types.CacheMaxFansCount && len(fansModel) > 0 {
				fansModel = append(fansModel, &model.Follow{UserID: -1})
			}
			err = l.addCacheFans(context.Background(), in.UserId, fansModel)
		})
	}
	return ret, nil
}

func (l *FansListLogic) cacheFansUserIds(ctx context.Context, userId, cursor, pageSize int64) ([]int64, []int64, error) {
	key := userFansKey(userId)
	b, err := l.svcCtx.BizRedis.ExistsCtx(ctx, key)
	if err != nil {
		logx.Errorf("[cacheFansUserIds] BizRedis.ExistsCtx error: %v", err)
	}
	if b {
		err = l.svcCtx.BizRedis.ExpireCtx(ctx, key, userFansExpireTime)
		if err != nil {
			logx.Errorf("[cacheFansUserIds] BizRedis.ExpireCtx error: %v", err)
		}
	}
	pairs, err := l.svcCtx.BizRedis.ZrevrangebyscoreWithScoresAndLimitCtx(ctx, key, 0, cursor, 0, int(pageSize))
	if err != nil {
		logx.Errorf("[cacheFansUserIds] BizRedis.ZrevrangebyscoreWithScoresAndLimitCtx error: %v", err)
		return nil, nil, err
	}
	var uids []int64
	var createTimes []int64
	for _, pair := range pairs {
		uid, err := strconv.ParseInt(pair.Key, 10, 64)
		createTime, err := strconv.ParseInt(strconv.FormatInt(pair.Score, 10), 10, 64)
		if err != nil {
			logx.Errorf("[cacheFansUserIds] strconv.ParseInt error: %v", err)
			continue
		}
		uids = append(uids, uid)
		createTimes = append(createTimes, createTime)
	}
	return uids, createTimes, nil
}

func (l *FansListLogic) addCacheFans(ctx context.Context, userId int64, fans []*model.Follow) error {
	if len(fans) == 0 {
		return nil
	}
	key := userFansKey(userId)
	for _, fan := range fans {
		var score int64
		if fan.UserID == -1 {
			score = 0
		} else {
			score = fan.CreateTime.Unix()
		}
		_, err := l.svcCtx.BizRedis.ZaddCtx(ctx, key, score, strconv.FormatInt(fan.UserID, 10))
		if err != nil {
			logx.Errorf("[addCacheFans] BizRedis.ZaddCtx error: %v", err)
			return err
		}
	}

	return l.svcCtx.BizRedis.ExpireCtx(ctx, key, userFollowExpireTime)
}
