import React from 'react';
import './App.scss';
import { createStore, StoreProvider } from 'store';
import { Layout } from 'components/layout';
import Pages from 'components/Pages';
import { ThemeProvider } from 'components/theme';

const App = () => {
  const store = createStore();

  return (
    <StoreProvider store={store}>
      <ThemeProvider>
        <Layout>
          <Pages />
        </Layout>
      </ThemeProvider>
    </StoreProvider>
  );
};

export default App;
