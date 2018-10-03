const conversionRate = 100000000;
let blocks, mempoolSubsidy, blocksHolder;

function figureNumberOfBlocksToDisplay() {
    const pageContentPaddingMargin = $('body').outerHeight(true) - $('body').height();
    const availableHeight = $(window).height() - pageContentPaddingMargin - ($('.blocks-holder').position().top * 1.5); // 200px for other padding and margin before
    const availableWidth = blocksHolder.outerWidth();

    const blockWidth = $('.blocks-holder > .decredblockWrap').outerWidth(true);
    const blockHeight = $('.blocks-holder > .decredblockWrap').outerHeight(true);

    const maxBlocksPerRow = Math.floor(availableWidth / blockWidth);
    let maxBlockRows = Math.round(availableHeight / blockHeight);
    let nMaxBlockElements = maxBlocksPerRow * maxBlockRows;

    while (nMaxBlockElements > blocks.length) {
        maxBlockRows--;
        nMaxBlockElements = maxBlocksPerRow * maxBlockRows;
    }

    const blockElements = $('.blocks-holder > .decredblockWrap');
    const nCurrentblockElements = blockElements.length;

    if (nCurrentblockElements > nMaxBlockElements) {
        // remove the last x blocks
        for (let i = nCurrentblockElements; i > nMaxBlockElements; i--) {
            blockElements[i - 1].remove();
        }
    }
    else {
        // add more blocks to fill display
        for (let i = nCurrentblockElements; i < nMaxBlockElements; i++) {
            const newBlockElement = newBlockHtmlElement(blocks[i]);
            blocksHolder.append(newBlockElement);
        }
    }

    setupTooltips();
}

function displayBlocks() {
    blocksHolder = $('.blocks-holder');
    blocksHolder.append(makeMempoolBlock(blocks[0]));

    const blockHtmlElements = blocks.slice(1).map(newBlockHtmlElement).join("\n");
    blocksHolder.append(blockHtmlElements);

    figureNumberOfBlocksToDisplay();
    window.addEventListener('resize', () => {
        figureNumberOfBlocksToDisplay();
    });
}

function makeMempoolBlock(block) {
    let fees = 0;
    for (const tx of block.Transactions) {
        fees += tx.Fees;
    }

    return `<div id="mempool-info" class="decredblockWrap">
                <div class="decredblock">
                    <div class="info-block">
                        <a class="color-code" href="/mempool">Mempool</a>
                        <div class="mono" style="line-height: 1;">${Math.floor(block.Total)} DCR</div>
                        <span class="timespan">
                            <span data-target="main.age" data-age="${block.Time}"></span>&nbsp;ago
                        </span>
                    </div>
                    <div class="block-rows">
                        ${makeRewardsElement(mempoolSubsidy, fees, block.Votes.length, '#')}
                        ${makeVoteElements(block.Votes)}
                        ${makeTicketAndRevoctionElements(block.Tickets, block.Revocations)}
                        ${makeTransactionElements(block.Transactions)}
                    </div>
                </div>
            </div>`;
}

function newBlockHtmlElement(block) {
    let rewardTxId;
    for (const tx of block.Transactions) {
        if (tx.Coinbase) {
            rewardTxId = tx.TxID;
            break;
        }
    }

    return `<div class="block-info decredblockWrap">
                <div class="decredblock">
                    ${makeBlockSummary(block.Height, block.TotalSent, block.Time)}
                    <div class="block-rows">
                        ${makeRewardsElement(block.Subsidy, block.MiningFee, block.Votes.length, rewardTxId)}
                        ${makeVoteElements(block.Votes)}
                        ${makeTicketAndRevoctionElements(block.Tickets, block.Revocations)}
                        ${makeTransactionElements(block.Transactions)}
                    </div>
                </div>
            </div>`;
}

function makeBlockSummary(blockHeight, totalSent, time) {
    return `<div class="info-block">
                <a class="color-code" href="/block/${blockHeight}">${blockHeight}</a>
                <div class="mono" style="line-height: 1;">${Math.floor(totalSent)} DCR</div>
                <span class="timespan">
                    <span data-target="main.age" data-age="${time}"></span>&nbsp;ago
                </span>
            </div>`;
}

function makeRewardsElement(subsidy, fee, voteCount, rewardTxId) {
    if (!subsidy) {
        return `<div class="block-rewards">
                    <span class="pow"><span class="paint" style="width:100%;"></span></span>
                    <span class="pos"><span class="paint" style="width:100%;"></span></span>
                    <span class="fund"><span class="paint" style="width:100%;"></span></span>
                    <span class="fees" title='{"object": "Tx Fees", "total": "${fee}"}'></span>
                </div>`;
    }

    const pow = subsidy.pow / conversionRate;
    const pos = subsidy.pos / conversionRate;
    const fund = (subsidy.developer || subsidy.dev) / conversionRate;
    
    const backgroundColorRelativeToVotes = `style="width: ${voteCount * 20}%"`; // 5 blocks = 100% painting

    // const totalDCR = Math.round(pow + fund + fee);
    const totalDCR = 1;
    return `<div class="block-rewards" style="flex-grow: ${totalDCR}">
                <span class="pow" style="flex-grow: ${pow}"
                    title='{"object": "PoW Reward", "total": "${pow}"}'>
                    <a href="/tx/${rewardTxId}">
                        <span class="paint" ${backgroundColorRelativeToVotes}></span>
                    </a>
                </span>
                <span class="pos" style="flex-grow: ${pos}"
                    title='{"object": "PoS Reward", "total": "${pos}"}'>
                    <a href="/tx/${rewardTxId}">
                        <span class="paint" ${backgroundColorRelativeToVotes}></span>
                    </a>
                </span>
                <span class="fund" style="flex-grow: ${fund}"
                    title='{"object": "Project Fund", "total": "${fund}"}'>
                    <a href="/tx/${rewardTxId}">
                        <span class="paint" ${backgroundColorRelativeToVotes}></span>
                    </a>
                </span>
                <span class="fees" style="flex-grow: ${fee}"
                    title='{"object": "Tx Fees", "total": "${fee}"}'>
                    <a href="/tx/${rewardTxId}"></a>
                </span>
            </div>`;
}

function makeVoteElements(votes) {
    let totalDCR = 0;
    const voteElements = (votes || []).map(vote => {
        totalDCR += vote.Total;
        return `<span style="background-color: ${vote.VoteValid ? '#2971ff' : 'rgba(253, 113, 74, 0.8)' }"
                    title='{"object": "Vote", "total": "${vote.Total}", "vote": "${vote.VoteValid}"}'>
                    <a href="/tx/${vote.TxID}"></a>
                </span>`;
    });

    // append empty squares to votes
    for (var i = voteElements.length; i < 5; i++) {
        voteElements.push('<span title="Empty vote slot"></span>');
    }

    // totalDCR = Math.round(totalDCR);
    totalDCR = 1;
    return `<div class="block-votes" style="flex-grow: ${totalDCR}">
                ${voteElements.join("\n")}
            </div>`;
}

function makeTicketAndRevoctionElements(tickets, revocations) {
    let totalDCR = 0;

    const ticketElements = (tickets || []).map(ticket => {
        totalDCR += ticket.Total;
        return makeTxElement(ticket, "block-ticket", "Ticket");
    });
    const revocationElements = (revocations || []).map(revocation => {
        totalDCR += revocation.Total;
        return makeTxElement(revocation, "block-rev", "Revocation");
    });

    const ticketsAndRevocationElements = ticketElements.concat(revocationElements);

    // append empty squares to tickets+revs
    for (var i = ticketsAndRevocationElements.length; i < 20; i++) {
        ticketsAndRevocationElements.push('<span title="Empty ticket slot"></span>');
    }

    // totalDCR = Math.round(totalDCR);
    totalDCR = 1;
    return `<div class="block-tickets" style="flex-grow: ${totalDCR}">
                ${ticketsAndRevocationElements.join("\n")}
            </div>`;
}

function makeTransactionElements(transactions) {
    let totalDCR = 0;
    const transactionElements = (transactions || [])
                .filter(tx => !tx.Coinbase)
                .map(tx => {
                    totalDCR += tx.Total;
                    return makeTxElement(tx, "block-tx", "Transaction", true);
                });

    // totalDCR = Math.round(totalDCR);
    totalDCR = 1;
    return `<div class="block-transactions" style="flex-grow: ${totalDCR}">
                ${transactionElements.join("\n")}
            </div>`;
}

function makeTxElement(tx, className, type, appendFlexGrow) {
    // const style = [ `opacity: ${(tx.VinCount + tx.VoutCount) / 10}` ];
    const style = [];
    if (appendFlexGrow) {
        style.push(`flex-grow: ${Math.round(tx.Total)}`);
    }

    return `<span class="${className}" style="${style.join("; ")}"
                title='{"object": "${type}", "total": "${tx.Total}", "vout": "${tx.VoutCount}", "vin": "${tx.VinCount}"}'>
                <a href="/tx/${tx.TxID}"></a>
            </span>`;
}

function setupTooltips() {
    // check for emtpy tx rows and set custom tooltip
    $('.block-transactions').each(function() {
        var blockTx = $(this);
        if (blockTx.children().length === 0) {
            blockTx.attr('title', 'No regular transaction in block');
        }
    })

    $('.block-rows [title]').each(function() {
        var tooltipElement = $(this);
        try {
            // parse the content
            var data = JSON.parse(tooltipElement.attr('title'));
            var newContent;
            if (data.object === "Vote") {
                newContent = `<b>${data.object} (${(data.vote === "true") ? "Yes" : "No"})</b><br>${data.total} DCR`;
            }
            else {
                newContent = `<b>${data.object}</b><br>${data.total} DCR`;
            }

            if (data.vin && data.vout) {
                newContent += `<br>${data.vin} Inputs, ${data.vout} Outputs`
            }
            
            tooltipElement.attr('title', newContent);
        }
        catch (error) {}
    })

    tippy('.block-rows [title]', {
        allowTitleHTML: true,
        animation: 'shift-away',
        arrow: true,
        createPopperInstanceOnInit: true,
        dynamicTitle: true,
        performance: true,
        placement: 'top',
        size: 'small',
        sticky: true,
        theme: 'light'
    })
}