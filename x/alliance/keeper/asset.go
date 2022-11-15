package keeper

import (
	"github.com/cosmos/cosmos-sdk/store"
	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/terra-money/alliance/x/alliance/types"
	"math"
)

// UpdateAllianceAsset updates the alliance asset with new params
// Also saves a snapshot whenever rewards weight changes to make sure delegation reward calculation has reference to
// historical reward rates
func (k Keeper) UpdateAllianceAsset(ctx sdk.Context, newAsset types.AllianceAsset) error {
	asset, found := k.GetAssetByDenom(ctx, newAsset.Denom)
	if !found {
		return types.ErrUnknownAsset
	}

	var err error
	// Only add a snapshot if reward weight changes
	if !newAsset.RewardWeight.Equal(asset.RewardWeight) {
		k.IterateAllianceValidatorInfo(ctx, func(valAddr sdk.ValAddress, info types.AllianceValidatorInfo) bool {
			validator, err := k.GetAllianceValidator(ctx, valAddr)
			if err != nil {
				return true
			}
			_, err = k.ClaimValidatorRewards(ctx, validator)
			if err != nil {
				return true
			}
			k.SetRewardWeightChangeSnapshot(ctx, asset, validator)
			return false
		})
		if err != nil {
			return err
		}
		// Queue a re-balancing event if reward weight change
		k.QueueAssetRebalanceEvent(ctx)
	}

	// If there was a change in reward decay rate or reward decay time
	if !newAsset.RewardChangeRate.Equal(asset.RewardChangeRate) || newAsset.RewardChangeInterval != asset.RewardChangeInterval {
		// And if there were no reward changes scheduled previously, start the counter from now
		if asset.RewardChangeRate.IsZero() || asset.RewardChangeInterval == 0 {
			asset.LastRewardChangeTime = ctx.BlockTime()
		}
		// Else do nothing since there is already a change that was scheduled.
		// The next trigger will use the new reward change and reward interval
		// following triggers will be scheduled using the new reward change interval
	}

	// Make sure only whitelisted fields can be updated
	asset.TakeRate = newAsset.TakeRate
	asset.RewardWeight = newAsset.RewardWeight
	asset.RewardChangeRate = newAsset.RewardChangeRate
	asset.RewardChangeInterval = newAsset.RewardChangeInterval
	k.SetAsset(ctx, asset)

	return nil
}

func (k Keeper) RebalanceHook(ctx sdk.Context, assets []*types.AllianceAsset) error {
	if k.ConsumeAssetRebalanceEvent(ctx) {
		return k.RebalanceBondTokenWeights(ctx, assets)
	}
	return nil
}

// RebalanceBondTokenWeights uses asset reward weights to calculate the expected amount of staking token that has to be
// minted / burned to maintain the right ratio
// It iterates all validators and calculates the expected staked amount based on delegations and delegates/undelegates
// the difference.
func (k Keeper) RebalanceBondTokenWeights(ctx sdk.Context, assets []*types.AllianceAsset) (err error) {
	moduleAddr := k.accountKeeper.GetModuleAddress(types.ModuleName)
	allianceBondAmount := k.getAllianceBondedAmount(ctx, moduleAddr)

	nativeBondAmount := k.stakingKeeper.TotalBondedTokens(ctx).Sub(allianceBondAmount)
	bondDenom := k.stakingKeeper.BondDenom(ctx)

	unbondedValidatorShares := sdk.NewDecCoins()
	var bondedValidators []types.AllianceValidator

	// Iterate through all alliance validators to remove those that are unbonded.
	// Unbonded validators will be ignored when rebalancing.
	k.IterateAllianceValidatorInfo(ctx, func(valAddr sdk.ValAddress, info types.AllianceValidatorInfo) bool {
		var validator types.AllianceValidator
		validator, err = k.GetAllianceValidator(ctx, valAddr)
		if err != nil {
			return true
		}
		if validator.IsBonded() {
			bondedValidators = append(bondedValidators, validator)
		} else {
			unbondedValidatorShares = unbondedValidatorShares.Add(validator.ValidatorShares...)
		}
		return false
	})
	if err != nil {
		return err
	}

	for _, validator := range bondedValidators {
		currentBondedAmount := sdk.NewDec(0)
		delegation, found := k.stakingKeeper.GetDelegation(ctx, moduleAddr, validator.GetOperator())
		if found {
			currentBondedAmount = validator.TokensFromShares(delegation.GetShares())
		}

		expectedBondAmount := sdk.ZeroDec()
		for _, asset := range assets {
			// Ignores assets that were recently added to prevent a small set of stakers from owning too much of the
			// voting power
			if ctx.BlockTime().Before(asset.RewardStartTime) {
				continue
			}
			valShares := validator.ValidatorSharesWithDenom(asset.Denom)
			expectedBondAmountForAsset := asset.RewardWeight.MulInt(nativeBondAmount)

			bondedValidatorShares := asset.TotalValidatorShares.Sub(unbondedValidatorShares.AmountOf(asset.Denom))
			if valShares.IsPositive() && bondedValidatorShares.IsPositive() {
				expectedBondAmount = expectedBondAmount.Add(valShares.Mul(expectedBondAmountForAsset).Quo(bondedValidatorShares))
			}
		}
		if expectedBondAmount.GT(currentBondedAmount) {
			// delegate more tokens to increase the weight
			bondAmount := expectedBondAmount.Sub(currentBondedAmount).TruncateInt()
			err = k.bankKeeper.MintCoins(ctx, types.ModuleName, sdk.NewCoins(sdk.NewCoin(bondDenom, bondAmount)))
			if err != nil {
				return nil
			}
			_, err = k.stakingKeeper.Delegate(ctx, moduleAddr, bondAmount, stakingtypes.Unbonded, *validator.Validator, true)
			if err != nil {
				return err
			}
		} else if expectedBondAmount.LT(currentBondedAmount) {
			// undelegate more tokens to reduce the weight
			unbondAmount := currentBondedAmount.Sub(expectedBondAmount).TruncateInt()
			sharesToUnbond, err := k.stakingKeeper.ValidateUnbondAmount(ctx, moduleAddr, validator.GetOperator(), unbondAmount)
			if err != nil {
				return err
			}
			tokensToBurn, err := k.stakingKeeper.Unbond(ctx, moduleAddr, validator.GetOperator(), sharesToUnbond)
			if err != nil {
				return err
			}
			err = k.bankKeeper.BurnCoins(ctx, stakingtypes.BondedPoolName, sdk.NewCoins(sdk.NewCoin(bondDenom, tokensToBurn)))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// SetAsset Does not check if the asset already exists and overwrites it
func (k Keeper) SetAsset(ctx sdk.Context, asset types.AllianceAsset) {
	store := ctx.KVStore(k.storeKey)
	b := k.cdc.MustMarshal(&asset)
	store.Set(types.GetAssetKey(asset.Denom), b)
}

func (k Keeper) GetAllAssets(ctx sdk.Context) (assets []*types.AllianceAsset) {
	store := ctx.KVStore(k.storeKey)
	iter := sdk.KVStorePrefixIterator(store, types.AssetKey)
	defer iter.Close()

	for iter.Valid() {
		var asset types.AllianceAsset
		b := iter.Value()
		k.cdc.MustUnmarshal(b, &asset)
		assets = append(assets, &asset)
		iter.Next()
	}
	return assets
}

func (k Keeper) GetAssetByDenom(ctx sdk.Context, denom string) (asset types.AllianceAsset, found bool) {
	store := ctx.KVStore(k.storeKey)
	assetKey := types.GetAssetKey(denom)
	b := store.Get(assetKey)

	if b == nil {
		return asset, false
	}

	k.cdc.MustUnmarshal(b, &asset)
	return asset, true
}

func (k Keeper) DeleteAsset(ctx sdk.Context, denom string) {
	store := ctx.KVStore(k.storeKey)
	assetKey := types.GetAssetKey(denom)
	store.Delete(assetKey)
}

func (k Keeper) QueueAssetRebalanceEvent(ctx sdk.Context) {
	store := ctx.KVStore(k.storeKey)
	key := types.AssetRebalanceQueueKey
	store.Set(key, []byte{0x00})
}

func (k Keeper) ConsumeAssetRebalanceEvent(ctx sdk.Context) bool {
	store := ctx.KVStore(k.storeKey)
	key := types.AssetRebalanceQueueKey
	b := store.Get(key)
	if b == nil {
		return false
	}
	store.Delete(key)
	return true
}

func (k Keeper) SetRewardWeightChangeSnapshot(ctx sdk.Context, asset types.AllianceAsset, val types.AllianceValidator) {
	snapshot := types.NewRewardWeightChangeSnapshot(asset, val)
	k.setRewardWeightChangeSnapshot(ctx, asset.Denom, val.GetOperator(), uint64(ctx.BlockHeight()), snapshot)
}

func (k Keeper) setRewardWeightChangeSnapshot(ctx sdk.Context, denom string, valAddr sdk.ValAddress, height uint64, snapshot types.RewardWeightChangeSnapshot) {
	key := types.GetRewardWeightChangeSnapshotKey(denom, valAddr, height)
	store := ctx.KVStore(k.storeKey)
	b := k.cdc.MustMarshal(&snapshot)
	store.Set(key, b)
}

func (k Keeper) IterateWeightChangeSnapshot(ctx sdk.Context, denom string, valAddr sdk.ValAddress, lastClaimHeight uint64) store.Iterator {
	store := ctx.KVStore(k.storeKey)
	key := types.GetRewardWeightChangeSnapshotKey(denom, valAddr, lastClaimHeight)
	end := types.GetRewardWeightChangeSnapshotKey(denom, valAddr, math.MaxUint64)
	return store.Iterator(key, end)
}

func (k Keeper) IterateAllWeightChangeSnapshot(ctx sdk.Context, cb func(denom string, valAddr sdk.ValAddress, lastClaimHeight uint64, snapshot types.RewardWeightChangeSnapshot) (stop bool)) {
	store := ctx.KVStore(k.storeKey)
	iter := sdk.KVStorePrefixIterator(store, types.RewardWeightChangeSnapshotKey)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		var snapshot types.RewardWeightChangeSnapshot
		k.cdc.MustUnmarshal(iter.Value(), &snapshot)
		denom, valAddr, height := types.ParseRewardWeightChangeSnapshotKey(iter.Key())
		if cb(denom, valAddr, height, snapshot) {
			return
		}
	}
}

func (k Keeper) RewardWeightDecayHook(ctx sdk.Context, assets []*types.AllianceAsset) {
	for _, asset := range assets {
		// If no reward changes are required, skip
		if asset.RewardChangeInterval == 0 || asset.RewardChangeRate.IsZero() {
			continue
		}
		// If it is not scheduled for change, skip
		if ctx.BlockTime().Before(asset.LastRewardChangeTime.Add(asset.RewardChangeInterval)) {
			continue
		}
		durationSinceLastClaim := ctx.BlockTime().Sub(asset.LastRewardChangeTime)
		rate := sdk.NewDec(durationSinceLastClaim.Nanoseconds()).Quo(sdk.NewDec(YEAR_IN_NANOS)).Mul(asset.RewardChangeRate)
		asset.RewardWeight = asset.RewardWeight.Mul(rate)
		asset.LastRewardChangeTime = ctx.BlockTime()
		k.SetAsset(ctx, *asset)
	}
}
