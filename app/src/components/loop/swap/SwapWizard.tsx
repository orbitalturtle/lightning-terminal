import React, { ReactNode } from 'react';
import { observer } from 'mobx-react-lite';
import { BuildSwapSteps } from 'types/state';
import { usePrefixedTranslation } from 'hooks';
import { useStore } from 'store';
import { ArrowLeft } from 'components/base';
import Tip from 'components/common/Tip';
import { styled } from 'components/theme';
import SwapConfigStep from './SwapConfigStep';
import SwapProcessingStep from './SwapProcessing';
import SwapReviewStep from './SwapReviewStep';

const Styled = {
  Wrapper: styled.section`
    display: flex;
    min-height: 360px;
    padding: 30px;
    background-color: ${props => props.theme.colors.darkBlue};
    border-radius: 35px;
    box-shadow: 0 2px 8px 0 rgba(0, 0, 0, 0.5);
  `,
  Nav: styled.div`
    width: 36px;
  `,
  Content: styled.div`
    flex-grow: 1;
    display: flex;
    align-items: stretch;
    flex-direction: row;
  `,
};

const SwapWizard: React.FC = () => {
  const { l } = usePrefixedTranslation('cmps.loop.swap.SwapWizard');

  const { buildSwapStore } = useStore();

  let cmp: ReactNode;
  switch (buildSwapStore.currentStep) {
    case BuildSwapSteps.ChooseAmount:
      cmp = <SwapConfigStep />;
      break;
    case BuildSwapSteps.ReviewQuote:
      cmp = <SwapReviewStep />;
      break;
    case BuildSwapSteps.Processing:
      cmp = <SwapProcessingStep />;
      break;
    default:
      return null;
  }

  const { Wrapper, Nav, Content } = Styled;
  return (
    <Wrapper>
      <Nav>
        <Tip overlay={l('backTip')}>
          <ArrowLeft onClick={buildSwapStore.goToPrevStep} />
        </Tip>
      </Nav>
      <Content>{cmp}</Content>
    </Wrapper>
  );
};

export default observer(SwapWizard);
