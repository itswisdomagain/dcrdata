function handleMempoolUpdate(evt) {
	const mempool = JSON.parse(evt);
	mempool.Time = Date.now() / 1000;
	const mempoolElement = makeMempoolBlock(mempool);
    $(mempoolElement).insertAfter($('.blocks-holder > *:first-child'));
    $('.blocks-holder > *:first-child').remove();
    setupTooltips();
}

function handleBlockUpdate(block) {
    const trimmedBlockInfo = trimBlockInfo(block);
    blocks.push(trimmedBlockInfo);
    $(newBlockHtmlElement(trimmedBlockInfo)).insertAfter($('.blocks-holder > *:first-child'));
    $('.blocks-holder > *:last-child').remove();
    setupTooltips();
}

function trimTxInfo(txs) {
	const trimmedTxs = [];
	for (const tx of txs) {
		const voteValid = !tx.VoteInfo ? false : tx.VoteInfo.block_validation.validity;
		const trimmedTx = {
			VinCount:  tx.Vin.length,
			VoutCount: tx.Vout.length,
			VoteValid: voteValid,
			TxID:      tx.TxID,
			Total:     tx.Total,
		};
		trimmedTxs.push(trimmedTx);
	}
	return trimmedTxs;
}

function trimBlockInfo(block) {
	return {
		Time:         block.time,
		Height:       block.height,
		TotalSent:    block.TotalSent,
		MiningFee:    block.MiningFee,
		Subsidy:      block.Subsidy,
		Votes:        trimTxInfo(block.Votes),
		Tickets:      trimTxInfo(block.Tickets),
		Revocations:  trimTxInfo(block.Revs),
		Transactions: trimTxInfo(block.Tx),
	}
}