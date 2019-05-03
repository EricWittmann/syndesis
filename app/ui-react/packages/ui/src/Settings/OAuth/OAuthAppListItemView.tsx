import * as React from 'react';
import { PageSection } from '../../Layout';
import { IListViewToolbarProps, ListViewToolbar } from '../../Shared';

export interface IOAuthAppListItemViewProps extends IListViewToolbarProps {
  children: React.ReactNode;
}

export const OAuthAppListItemView: React.FunctionComponent<
  IOAuthAppListItemViewProps
> = ({ children, ...rest }) => (
  <>
    <PageSection noPadding={true} variant={'light'}>
      <ListViewToolbar {...rest} />
    </PageSection>
    <PageSection>{children}</PageSection>
  </>
);
